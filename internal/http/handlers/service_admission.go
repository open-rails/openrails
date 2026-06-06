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
	InvokerID       string                           `json:"invoker_id"`
	Tier            string                           `json:"tier"`
	Model           string                           `json:"model"`
	Amounts         map[string]int64                 `json:"amounts"`
	CreditType      string                           `json:"credit_type"`
	EstimateCents   int64                            `json:"estimate_cents"`
	RequestID       string                           `json:"request_id"`
	Source          string                           `json:"source"`
	ExpiresAt       *int64                           `json:"expires_at"`
	BlockChecks     []billingservice.AdmitBlockCheck `json:"block_checks"`
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
	invoker := strings.TrimSpace(req.InvokerID)
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
		TenantSubjectID: *payer,
		InvokerID:       invoker,
		Tier:            req.Tier,
		Model:           req.Model,
		Amounts:         req.Amounts,
		CreditType:      req.CreditType,
		EstimateCents:   req.EstimateCents,
		Source:          req.Source,
		SourceID:        req.RequestID,
		BlockChecks:     req.BlockChecks,
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

// ServiceGetBudget returns the invoker's rolling money-budget windows (#304
// introspection) for a host's /status dashboard. tenant_subject_id + invoker_id + tier are
// query params; the tenant is pinned from the OAT.
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
	invoker := r.Query("invoker_id")
	windows, err := svc.BudgetStatus(r.Request.Context(), *payer, invoker, r.Query("tier"))
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "budget lookup failed")
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
