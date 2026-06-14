package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingidentity "github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceAdmitRequest struct {
	CustomerID     string                           `json:"customer_id"`
	Actor          string                           `json:"actor"`
	Tier           string                           `json:"tier"`
	Resource       string                           `json:"resource"`
	Amounts        map[string]int64                 `json:"amounts"`
	EstimateMicros int64                            `json:"estimate_micros"`
	RequestID      string                           `json:"request_id"`
	Source         string                           `json:"source"`
	ExpiresAt      *int64                           `json:"expires_at"`
	BlockChecks    []billingservice.AdmitBlockCheck `json:"block_checks"`
	// Roles are the actor's immutable role UUIDs (#473) — each (subject, role)
	// budget-scope policy gates this request's spend.
	Roles []uuid.UUID `json:"roles"`
	// #404: per-tenant fixed-window throughput the host wants OpenRails to enforce.
	TenantThroughput []billingservice.AdmitThroughputWindow `json:"tenant_throughput"`
}

// ServiceAdmit is the unified admission endpoint (issue #298): throughput +
// money + suspension + blocklist + endpoint gating in one decision. It emits
// x-ratelimit-* headers and returns 429 + Retry-After on a throughput breach,
// 402 on a money deny, 403 on suspended/blocked/endpoint, 200 when allowed.
func ServiceAdmit(r *httprequest.Request) {
	var req serviceAdmitRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.EstimateMicros < 0 {
		r.ErrorJSON(http.StatusBadRequest, "estimate_micros must be >= 0")
		return
	}
	actor := strings.TrimSpace(req.Actor)
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	in := admitInputFromRequest(req, *payer, actor)

	res, err := svc.Admit(r.Request.Context(), in)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "admission check failed")
		return
	}

	// x-ratelimit-* headers (per unit window).
	for _, w := range res.Windows {
		r.SetHeader("X-RateLimit-Limit-"+w.Unit, strconv.FormatInt(w.Limit, 10))
		r.SetHeader("X-RateLimit-Remaining-"+w.Unit, strconv.FormatInt(w.Remaining, 10))
		r.SetHeader("X-RateLimit-Reset-"+w.Unit, strconv.FormatInt(w.ResetAfterSeconds, 10))
	}

	if res.Allowed {
		r.JSON(http.StatusOK, res)
		return
	}
	switch res.BlockedBy {
	case "throughput", "abuse":
		if res.RetryAfterSeconds > 0 {
			r.SetHeader("Retry-After", strconv.FormatInt(res.RetryAfterSeconds, 10))
		}
		r.JSON(http.StatusTooManyRequests, res)
	case "money":
		r.JSON(http.StatusPaymentRequired, res)
	default: // suspended, blocked, endpoint
		r.JSON(http.StatusForbidden, res)
	}
}

// admitInputFromRequest maps one admit body onto the service-facade input —
// shared by the single /admit route and each /admit/batch item.
func admitInputFromRequest(req serviceAdmitRequest, payer billingidentity.CustomerID, actor string) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		CustomerID:       payer,
		Actor:            actor,
		Tier:             req.Tier,
		Resource:         req.Resource,
		Amounts:          req.Amounts,
		EstimateMicros:   req.EstimateMicros,
		Source:           req.Source,
		SourceID:         req.RequestID,
		Roles:            req.Roles,
		BlockChecks:      req.BlockChecks,
		TenantThroughput: req.TenantThroughput,
	}
	if req.ExpiresAt != nil {
		in.ExpiresAtUnix = *req.ExpiresAt
	}
	return in
}

// admitVerdictStatus maps an admission result onto the HTTP status the single
// /admit route would return — reused per-item by the batch route.
func admitVerdictStatus(res *billingservice.AdmitResult) int {
	if res.Allowed {
		return http.StatusOK
	}
	switch res.BlockedBy {
	case "throughput", "abuse":
		return http.StatusTooManyRequests
	case "money":
		return http.StatusPaymentRequired
	default: // suspended, blocked, unverified, endpoint
		return http.StatusForbidden
	}
}

type serviceAdmitBatchRequest struct {
	Items []serviceAdmitRequest `json:"items"`
}

// serviceAdmitVerdict is one per-item batch outcome (#335). Status is the
// HTTP-equivalent status the single /admit route would have returned for this
// item; Result carries the full admission decision when one was reached.
type serviceAdmitVerdict struct {
	Status int                         `json:"status"`
	Error  string                      `json:"error,omitempty"`
	Result *billingservice.AdmitResult `json:"result,omitempty"`
}

// serviceAdmitBatchVerdicts runs the per-item admission loop with FULL per-item
// isolation: a bad payer id, scope denial, deny, or backend error on one item
// never fails the others. allows gates each item's payer against the service
// token's tenant-subject scope; admit is the (injected) single-admit core.
//
// Follow-up (#335): each item currently runs the full single-admit path
// (suspension/settings/balance queries per item). An obvious optimization is
// batching the shared reads (suspension + settings per distinct payer, one
// Redis pipeline for throughput) inside one transaction — deferred for v1.
func serviceAdmitBatchVerdicts(
	ctx context.Context,
	items []serviceAdmitRequest,
	allows func(billingidentity.CustomerID) bool,
	admit func(context.Context, billingservice.AdmitInput) (*billingservice.AdmitResult, error),
) []serviceAdmitVerdict {
	out := make([]serviceAdmitVerdict, len(items))
	for i, item := range items {
		if item.EstimateMicros < 0 {
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "estimate_micros must be >= 0"}
			continue
		}
		payer, err := parseServiceCustomerID(item.CustomerID)
		if err != nil || payer == nil {
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "customer_id required"}
			continue
		}
		if !allows(*payer) {
			out[i] = serviceAdmitVerdict{Status: http.StatusForbidden, Error: "service_token_tenant_subject_scope_denied"}
			continue
		}
		res, err := admit(ctx, admitInputFromRequest(item, *payer, strings.TrimSpace(item.Actor)))
		if err != nil {
			out[i] = serviceAdmitVerdict{Status: http.StatusInternalServerError, Error: "admission check failed"}
			continue
		}
		out[i] = serviceAdmitVerdict{Status: admitVerdictStatus(res), Result: res}
	}
	return out
}

// ServiceAdmitBatch is the cross-payer batch admission endpoint (#335): one
// request carries N admit items (mixed payers); the response carries N
// positional verdicts with the same semantics as /v1/service/admit per item.
// The batch itself always answers 200 — per-item denial/errors live in the
// items, so cold payers conflating admits collapse N hops into one without one
// broke payer poisoning the flight.
func ServiceAdmitBatch(r *httprequest.Request) {
	var req serviceAdmitBatchRequest
	if !r.BindJSON(&req) {
		return
	}
	if len(req.Items) == 0 {
		r.ErrorJSON(http.StatusBadRequest, "items required")
		return
	}
	if len(req.Items) > maxWindowBatchItems {
		r.ErrorJSON(http.StatusBadRequest, "too many items")
		return
	}
	resolved, status, msg := serviceTokenFromRequest(r)
	if resolved == nil {
		r.ErrorJSON(status, msg)
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	verdicts := serviceAdmitBatchVerdicts(
		r.Request.Context(),
		req.Items,
		func(ts billingidentity.CustomerID) bool { return resolved.AllowsCustomer(ts.UUID()) },
		svc.Admit,
	)
	r.JSON(http.StatusOK, map[string]any{"items": verdicts})
}

// ServiceGetBudget returns the actor's fixed money-budget windows (#304
// introspection) for a host's /status dashboard. customer_id + actor + tier are
// query params; the tenant is pinned from the service token.
func ServiceGetBudget(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	actor := r.Query("actor")
	windows, err := svc.BudgetStatus(r.Request.Context(), *payer, actor, r.Query("tier"))
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "budget lookup failed")
		return
	}
	r.SuccessJSON(map[string]any{"windows": windows})
}

// ServiceGetTier returns the payer's CURRENT graduated tier (#477): the tier
// OpenRails auto-maintains from cumulative paid spend against the persisted tier
// schedule (#476). customer_id is a query param; the tenant is pinned from
// the service token. Empty tier => the payer has never graduated (caller treats
// it as the lowest/default). Operator service token, credits:read.
func ServiceGetTier(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	tier, err := svc.GetTier(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "tier lookup failed")
		return
	}
	r.SuccessJSON(map[string]any{"tier": tier})
}

type serviceReportWastedSpendRequest struct {
	CustomerID string `json:"customer_id"`
	Actor      string `json:"actor"`
	Micros     int64  `json:"micros"`
	Reason     string `json:"reason"`
}

// ServiceReportWastedSpend records host-reported WASTED $ (#488): a failed
// attempt that cost the platform money. Accrues into the payer's per-tier
// bad_spend windows + the actor's flat windows; a later Admit denies when either
// is over budget. Operator service token, credits:write.
func ServiceReportWastedSpend(r *httprequest.Request) {
	var req serviceReportWastedSpendRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.Micros < 0 {
		r.ErrorJSON(http.StatusBadRequest, "micros must be >= 0")
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.ReportWastedSpend(r.Request.Context(), *payer, strings.TrimSpace(req.Actor), req.Micros, req.Reason); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "report wasted spend failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

// ServiceAbuseUsage returns the payer's + actor's running WASTED-$ totals +
// budgets (#488 introspection). customer_id + optional actor + tier are query
// params; the tenant is pinned from the service token. Operator service token,
// credits:read.
func ServiceAbuseUsage(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	pw, aw, err := svc.AbuseUsage(r.Request.Context(), *payer, r.Query("actor"), r.Query("tier"))
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "abuse usage lookup failed")
		return
	}
	r.JSON(http.StatusOK, map[string]any{"payer_windows": pw, "actor_windows": aw})
}

type serviceCreditLimitRequest struct {
	CustomerID        string `json:"customer_id"`
	CreditLimitMicros int64  `json:"credit_limit_micros"`
}

// ServiceSetCreditLimit sets the admin/operator arrears credit line for a payer
// (#489): under billing_mode=arrears the balance may go NEGATIVE up to the limit;
// AdmitHold denies insufficient_credit when a new hold would exceed it. 0 = off.
// OPERATOR-ADMIN gated at the route (openrails:admin) — NOT self-serve.
func ServiceSetCreditLimit(r *httprequest.Request) {
	var req serviceCreditLimitRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.CreditLimitMicros < 0 {
		r.ErrorJSON(http.StatusBadRequest, "credit_limit_micros must be >= 0")
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetCreditLimit(r.Request.Context(), *payer, req.CreditLimitMicros); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set credit limit failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

// ServiceGetCreditLimit returns the admin-set arrears credit line for a payer
// (#489). customer_id is a query param; the tenant is pinned from the service
// token. Operator service token, credits:read.
func ServiceGetCreditLimit(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	v, err := svc.GetCreditLimit(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "credit limit lookup failed")
		return
	}
	r.SuccessJSON(map[string]any{"credit_limit_micros": v})
}

type serviceBudgetCheckRequest struct {
	CustomerID      string                                  `json:"customer_id"`
	Actor           string                                  `json:"actor"`
	Windows         []billingservice.BudgetCheckWindowInput `json:"windows"`
	RequestedMicros int64                                   `json:"requested_micros"`
}

// ServiceBudgetCheck computes fixed money-budget windows for (payer, actor)
// against CALLER-SUPPLIED windows (the host owns the budget policy; OpenRails
// owns the spend actuals) WITHOUT reserving. Powers the tensorhub delegated
// budget-window display (#410). Operator service token, credits:read.
func ServiceBudgetCheck(r *httprequest.Request) {
	var req serviceBudgetCheckRequest
	if !r.BindJSON(&req) {
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	windows, err := svc.BudgetCheck(r.Request.Context(), *payer, req.Actor, req.Windows, req.RequestedMicros)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "budget check failed")
		return
	}
	r.SuccessJSON(map[string]any{"windows": windows})
}

type serviceTierPolicyRequest struct {
	CustomerID string `json:"customer_id"`
	billingservice.TierPolicyInput
}

// ServiceSetTierPolicy upserts a tier policy (#298 tier admin API): throughput
// windows + entitled endpoints + queue limits + fixed money-budget windows. An
// EMPTY customer_id sets the tenant-wide DEFAULT policy for the tier (#477,
// the platform capacity ladder declared once); a non-empty one sets a
// per-subject override (scope-checked).
func ServiceSetTierPolicy(r *httprequest.Request) {
	var req serviceTierPolicyRequest
	if !r.BindJSON(&req) {
		return
	}
	var payer billingidentity.CustomerID
	if strings.TrimSpace(req.CustomerID) != "" {
		p, err := parseServiceCustomerID(req.CustomerID)
		if err != nil || p == nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
			return
		}
		if !requireServiceCustomerScope(r, *p) {
			return
		}
		payer = *p
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetTierPolicy(r.Request.Context(), payer, req.TierPolicyInput); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set tier policy failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

type serviceTierScheduleRequest struct {
	CustomerID string                            `json:"customer_id"`
	Schedule   []billingservice.TierScheduleRung `json:"schedule"`
}

// ServiceSetTierSchedule persists the tenant's tier SCHEDULE (#476): the host
// declares the ladder ONCE and OpenRails AUTO-maintains each payer's tier from
// cumulative spend. An EMPTY customer_id sets the tenant-wide default
// schedule (the common case); a non-empty one sets a per-subject override
// (scope-checked). owner=platform. Operator service token, credits:write.
func ServiceSetTierSchedule(r *httprequest.Request) {
	var req serviceTierScheduleRequest
	if !r.BindJSON(&req) {
		return
	}
	var payer billingidentity.CustomerID
	if strings.TrimSpace(req.CustomerID) != "" {
		p, err := parseServiceCustomerID(req.CustomerID)
		if err != nil || p == nil {
			r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
			return
		}
		if !requireServiceCustomerScope(r, *p) {
			return
		}
		payer = *p
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetTierSchedule(r.Request.Context(), payer, req.Schedule); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set tier schedule failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

// budgetScopeWindow is one fixed money-budget window in a budget-scope policy
// request/response (#473) — same shape as billingservice.BudgetScopeWindowInput.
type budgetScopeWindow struct {
	Key           string `json:"key"`
	WindowSeconds int64  `json:"window_seconds"`
	LimitMicros   int64  `json:"limit_micros"`
	Cadence       string `json:"cadence,omitempty"`
}

func budgetScopeWindowInputs(ws []budgetScopeWindow) []billingservice.BudgetScopeWindowInput {
	out := make([]billingservice.BudgetScopeWindowInput, 0, len(ws))
	for _, w := range ws {
		out = append(out, billingservice.BudgetScopeWindowInput{
			Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence,
		})
	}
	return out
}

type serviceSubjectBudgetPolicyRequest struct {
	CustomerID string              `json:"customer_id"`
	Scope      string              `json:"scope"`
	RoleID     string              `json:"role_id"`
	Windows    []budgetScopeWindow `json:"windows"`
}

// ServiceSetSubjectBudgetPolicy upserts a SUBJECT-owned hierarchical
// budget-scope policy (#473): a self cap (scope=subject) or a (subject, role)
// pool (scope=role; role_id is the role uuid). The owner is forced to "subject"
// by the service facade. Operator service token, credits:write.
func ServiceSetSubjectBudgetPolicy(r *httprequest.Request) {
	var req serviceSubjectBudgetPolicyRequest
	if !r.BindJSON(&req) {
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetSubjectBudgetPolicy(r.Request.Context(), *payer, billingservice.BudgetScopePolicyInput{
		Scope:    req.Scope,
		ScopeKey: req.RoleID,
		Windows:  budgetScopeWindowInputs(req.Windows),
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set subject budget policy failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

type servicePlatformBudgetPolicyRequest struct {
	CustomerID string              `json:"customer_id"`
	Scope      string              `json:"scope"`
	Windows    []budgetScopeWindow `json:"windows"`
}

// ServiceSetPlatformBudgetPolicy upserts a PLATFORM-owned budget cap (#473): the
// platform->payer cap (scope=subject). The owner is forced to "platform" by the
// service facade. Platform-admin gated at the route (openrails:admin); a
// subject's own policy surface can never reach it.
func ServiceSetPlatformBudgetPolicy(r *httprequest.Request) {
	var req servicePlatformBudgetPolicyRequest
	if !r.BindJSON(&req) {
		return
	}
	payer, err := parseServiceCustomerID(req.CustomerID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetPlatformBudgetPolicy(r.Request.Context(), *payer, billingservice.BudgetScopePolicyInput{
		Scope:   req.Scope,
		Windows: budgetScopeWindowInputs(req.Windows),
	}); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set platform budget policy failed")
		return
	}
	r.SuccessJSONMessage("ok")
}

// ServiceGetSubjectBudgetPolicies returns ONLY the subject-owned budget-scope
// policies (#473) for host reconciliation — platform-owned caps are invisible.
// customer_id is a query param; the tenant is pinned from the service
// token. Operator service token, credits:read.
func ServiceGetSubjectBudgetPolicies(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Query("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "customer_id required")
		return
	}
	if !requireServiceCustomerScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	policies, err := svc.SubjectBudgetPolicies(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "subject budget policies lookup failed")
		return
	}
	out := make([]map[string]any, 0, len(policies))
	for _, p := range policies {
		ws := make([]budgetScopeWindow, 0, len(p.Windows))
		for _, w := range p.Windows {
			ws = append(ws, budgetScopeWindow{Key: w.Key, WindowSeconds: w.WindowSeconds, LimitMicros: w.LimitMicros, Cadence: w.Cadence})
		}
		out = append(out, map[string]any{"scope": p.Scope, "role_id": p.ScopeKey, "windows": ws})
	}
	r.SuccessJSON(map[string]any{"policies": out})
}
