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
	TenantSubjectID string                           `json:"tenant_subject_id"`
	Actor           string                           `json:"actor"`
	Tier            string                           `json:"tier"`
	Resource        string                           `json:"resource"`
	Amounts         map[string]int64                 `json:"amounts"`
	CreditType      string                           `json:"credit_type"`
	EstimateMicros  int64                            `json:"estimate_micros"`
	RequestID       string                           `json:"request_id"`
	Source          string                           `json:"source"`
	ExpiresAt       *int64                           `json:"expires_at"`
	BlockChecks     []billingservice.AdmitBlockCheck `json:"block_checks"`
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
	payer, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *payer) {
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
	case "throughput":
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
func admitInputFromRequest(req serviceAdmitRequest, payer billingidentity.TenantSubjectID, actor string) billingservice.AdmitInput {
	in := billingservice.AdmitInput{
		TenantSubjectID:  payer,
		Actor:            actor,
		Tier:             req.Tier,
		Resource:         req.Resource,
		Amounts:          req.Amounts,
		CreditType:       req.CreditType,
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
	case "throughput":
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
	allows func(billingidentity.TenantSubjectID) bool,
	admit func(context.Context, billingservice.AdmitInput) (*billingservice.AdmitResult, error),
) []serviceAdmitVerdict {
	out := make([]serviceAdmitVerdict, len(items))
	for i, item := range items {
		if item.EstimateMicros < 0 {
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "estimate_micros must be >= 0"}
			continue
		}
		payer, err := parseServiceTenantSubjectID(item.TenantSubjectID)
		if err != nil || payer == nil {
			out[i] = serviceAdmitVerdict{Status: http.StatusBadRequest, Error: "tenant_subject_id required"}
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
		func(ts billingidentity.TenantSubjectID) bool { return resolved.AllowsTenantSubject(ts.UUID()) },
		svc.Admit,
	)
	r.JSON(http.StatusOK, map[string]any{"items": verdicts})
}

// ServiceGetBudget returns the actor's fixed money-budget windows (#304
// introspection) for a host's /status dashboard. tenant_subject_id + actor + tier are
// query params; the tenant is pinned from the service token.
func ServiceGetBudget(r *httprequest.Request) {
	payer, err := parseServiceTenantSubjectID(r.Query("tenant_subject_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *payer) {
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

type serviceBudgetCheckRequest struct {
	TenantSubjectID string                                  `json:"tenant_subject_id"`
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
	payer, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *payer) {
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
	TenantSubjectID string `json:"tenant_subject_id"`
	billingservice.TierPolicyInput
}

// ServiceSetTierPolicy upserts a per-payer tier policy (#298 tier admin API):
// throughput windows + entitled endpoints + fixed money-budget windows.
func ServiceSetTierPolicy(r *httprequest.Request) {
	var req serviceTierPolicyRequest
	if !r.BindJSON(&req) {
		return
	}
	payer, err := parseServiceTenantSubjectID(req.TenantSubjectID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "tenant_subject_id required")
		return
	}
	if !requireServiceTenantSubjectScope(r, *payer) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.SetTierPolicy(r.Request.Context(), *payer, req.TierPolicyInput); err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "set tier policy failed")
		return
	}
	r.SuccessJSONMessage("ok")
}
