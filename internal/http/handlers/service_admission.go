package handlers

import (
	"net/http"
	"strconv"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceAdmitRequest struct {
	TenantSubjectID string                           `json:"tenant_subject_id"`
	Actor           string                           `json:"actor"`
	Tier            string                           `json:"tier"`
	Resource        string                           `json:"resource"`
	Amounts         map[string]int64                 `json:"amounts"`
	CreditType      string                           `json:"credit_type"`
	EstimateCents   int64                            `json:"estimate_cents"`
	RequestID       string                           `json:"request_id"`
	Source          string                           `json:"source"`
	ExpiresAt       *int64                           `json:"expires_at"`
	BlockChecks     []billingservice.AdmitBlockCheck `json:"block_checks"`
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
	if req.EstimateCents < 0 {
		r.ErrorJSON(http.StatusBadRequest, "estimate_cents must be >= 0")
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

	in := billingservice.AdmitInput{
		TenantSubjectID:  *payer,
		Actor:            actor,
		Tier:             req.Tier,
		Resource:         req.Resource,
		Amounts:          req.Amounts,
		CreditType:       req.CreditType,
		EstimateCents:    req.EstimateCents,
		Source:           req.Source,
		SourceID:         req.RequestID,
		BlockChecks:      req.BlockChecks,
		TenantThroughput: req.TenantThroughput,
	}
	if req.ExpiresAt != nil {
		in.ExpiresAtUnix = *req.ExpiresAt
	}

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

// ServiceGetBudget returns the actor's rolling money-budget windows (#304
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
	TenantSubjectID     string                                  `json:"tenant_subject_id"`
	Actor               string                                  `json:"actor"`
	Windows             []billingservice.BudgetCheckWindowInput `json:"windows"`
	RequestedMillicents int64                                   `json:"requested_millicents"`
}

// ServiceBudgetCheck computes rolling money-budget windows for (payer, actor)
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
	windows, err := svc.BudgetCheck(r.Request.Context(), *payer, req.Actor, req.Windows, req.RequestedMillicents)
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
// throughput windows + entitled endpoints + rolling money-budget windows.
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
