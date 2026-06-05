package handlers

import (
	"net/http"
	"strconv"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type serviceAdmitRequest struct {
	Payer         string                           `json:"payer"`
	OwnerID       string                           `json:"owner_id"`
	OwnerOrgID    string                           `json:"owner_org_id"`
	Actor         string                           `json:"actor"`
	Invoker       string                           `json:"invoker"`
	Tier          string                           `json:"tier"`
	Model         string                           `json:"model"`
	Amounts       map[string]int64                 `json:"amounts"`
	CreditType    string                           `json:"credit_type"`
	EstimateCents int64                            `json:"estimate_cents"`
	RequestID     string                           `json:"request_id"`
	Source        string                           `json:"source"`
	ExpiresAt     *int64                           `json:"expires_at"`
	BlockChecks   []billingservice.AdmitBlockCheck `json:"block_checks"`
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
	if actor == "" {
		actor = strings.TrimSpace(req.Invoker)
	}

	payerRaw := strings.TrimSpace(req.Payer)
	if payerRaw == "" {
		payerRaw = req.OwnerID
	}
	payer, err := parseServiceOwnerOrgID(payerRaw, req.OwnerOrgID)
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "payer required")
		return
	}

	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}

	in := billingservice.AdmitInput{
		Owner:         *payer,
		Actor:         actor,
		Tier:          req.Tier,
		Model:         req.Model,
		Amounts:       req.Amounts,
		CreditType:    req.CreditType,
		EstimateCents: req.EstimateCents,
		Source:        req.Source,
		SourceID:      req.RequestID,
		BlockChecks:   req.BlockChecks,
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
