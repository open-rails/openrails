package handlers

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
)

func CreateStripeMethodSetup(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	var req struct {
		PSPID   uuid.UUID `json:"psp_id"`
		Consent bool      `json:"consent"`
	}
	if !r.BindJSON(&req) {
		return
	}
	if !req.Consent {
		r.ErrorJSON(http.StatusBadRequest, "permission to save the card for future agreed payments is required")
		return
	}
	resolver, ok := stripeSetupResolver(r)
	if !ok {
		return
	}
	result, err := r.State.CheckoutService.CreateStripeMethodSetup(r.Request.Context(), req.PSPID, r.Header("Idempotency-Key"), checkoutVerifiedPrincipal(r), resolver)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(result)
}
func GetStripeMethodSetup(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	id, err := openrails.ParseCheckoutSessionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid setup id")
		return
	}
	resolver, ok := stripeSetupResolver(r)
	if !ok {
		return
	}
	result, err := r.State.CheckoutService.StripeMethodSetup(r.Request.Context(), id.UUID(), checkoutVerifiedPrincipal(r), resolver)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(result)
}
func ConfirmStripeMethodSetup(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	id, err := openrails.ParseCheckoutSessionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid setup id")
		return
	}
	resolver, ok := stripeSetupResolver(r)
	if !ok {
		return
	}
	result, err := r.State.CheckoutService.ConfirmStripeMethodSetup(r.Request.Context(), id.UUID(), checkoutVerifiedPrincipal(r), resolver)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(result)
}
func stripeSetupResolver(r *httprequest.Request) (intents.StripeEngineServiceResolver, bool) {
	if r.State != nil && r.State.CheckoutService != nil {
		if resolver, ok := r.State.CollectionResolver.(intents.StripeEngineServiceResolver); ok {
			return resolver, true
		}
	}
	r.ErrorJSON(http.StatusServiceUnavailable, "Stripe card setup unavailable")
	return nil, false
}
