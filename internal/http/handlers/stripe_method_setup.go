package handlers

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
)

// PaymentMethodSetupParams starts an in-page card setup with a PSP whose
// browser SDK collects the card. Consent is the customer's permission to
// charge the card for future agreed payments.
type PaymentMethodSetupParams struct {
	PSPID   uuid.UUID `json:"psp_id"`
	Consent bool      `json:"consent"`
}

// CreatePaymentMethodSetup (POST /me/payment-method-setups) starts a card
// setup.
func CreatePaymentMethodSetup(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	var req PaymentMethodSetupParams
	if !r.BindJSON(&req) {
		return
	}
	if !req.Consent {
		r.APIError(api.Coded(billing.CodeInvalidParam, "permission to save the card for future agreed payments is required").WithParam("consent"))
		return
	}
	if user := r.GetUser(); user != nil && refuseBlockedCardAttempt(r, user.ID) {
		return
	}
	resolver, ok := stripeSetupResolver(r)
	if !ok {
		return
	}
	result, err := r.State.CheckoutService.CreateStripeMethodSetup(r.Request.Context(), req.PSPID, r.Header("Idempotency-Key"), checkoutVerifiedPrincipal(r), resolver)
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return
	}
	r.SuccessJSON(result)
}

// GetPaymentMethodSetup (GET /me/payment-method-setups/{id}) reads a setup.
func GetPaymentMethodSetup(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	id, err := billing.ParseCheckoutAttemptID(r.Param("id"))
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
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return
	}
	r.SuccessJSON(result)
}

// ConfirmPaymentMethodSetup verifies a setup with the provider.
func ConfirmPaymentMethodSetup(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	id, err := billing.ParseCheckoutAttemptID(r.Param("id"))
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
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
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
