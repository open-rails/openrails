package handlers

import (
	"net/http"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// checkoutRoutingDryRunRequest is the dry-run body. Every field is a routing
// INPUT: nothing here creates or mutates anything.
type checkoutRoutingDryRunRequest struct {
	// PriceID accepts a price id or a price key (#774).
	PriceID string `json:"price_id"`
	// Country is the payer's country when known (ISO-3166-1 alpha-2).
	Country string `json:"country,omitempty"`
	// Selector traces an EXPLICITLY named PSP (the checkout payment.rail value).
	// Empty asks what the merchant's policy would pick on its own.
	Selector string `json:"selector,omitempty"`
}

type checkoutRoutingCandidateResponse struct {
	Selector string `json:"selector"`
	Rail     string `json:"rail,omitempty"`
	// Skip is absent when the candidate is eligible.
	Skip string `json:"skip,omitempty"`
}

type checkoutRoutingDryRunResponse struct {
	Object string `json:"object"`
	// Policy is who decided: explicit | merchant | default.
	Policy string `json:"policy"`
	// Rule is the matched merchant-rule index; absent for explicit/default.
	Rule *int `json:"rule,omitempty"`
	// Selected is the winning PSP key, empty when nothing was eligible.
	Selected   string                             `json:"selected,omitempty"`
	Rail       string                             `json:"rail,omitempty"`
	Mode       string                             `json:"mode,omitempty"`
	Candidates []checkoutRoutingCandidateResponse `json:"candidates"`
	// RoutingReason is the exact document a real session would persist on
	// checkout_sessions.routing_reason for this decision.
	RoutingReason any `json:"routing_reason,omitempty"`
}

// MerchantDryRunCheckoutRouting handles
// POST /v1/merchant/checkout-routing/dry-run (or#288): explain which PSP a
// checkout for this price would land on, and why every other candidate did not,
// without creating a session.
func MerchantDryRunCheckoutRouting(r *httprequest.Request) {
	var req checkoutRoutingDryRunRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	trace, err := svc.DryRunCheckoutRouting(r.Request.Context(), billingservice.CheckoutRoutingDryRun{
		PriceID:  req.PriceID,
		Country:  req.Country,
		Selector: req.Selector,
	})
	if err != nil {
		// Dry run reads only price/product/PSP state the caller supplied or
		// owns, so a failure is a bad input, not a server fault.
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	out := checkoutRoutingDryRunResponse{
		Object:     "checkout_routing_decision",
		Policy:     trace.Policy,
		Rule:       trace.Rule,
		Selected:   trace.Selected,
		Rail:       trace.Rail,
		Mode:       trace.Mode,
		Candidates: make([]checkoutRoutingCandidateResponse, 0, len(trace.Candidates)),
	}
	if trace.Reason != nil {
		out.RoutingReason = trace.Reason
	}
	for _, candidate := range trace.Candidates {
		out.Candidates = append(out.Candidates, checkoutRoutingCandidateResponse{
			Selector: candidate.Selector,
			Rail:     candidate.Rail,
			Skip:     candidate.Skip,
		})
	}
	r.SuccessJSON(out)
}
