package handlers

import (
	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
	"net/http"
)

// The self route and service both require the operation's interactive payer;
// merchant/service credentials cannot acquire a payment's client secret.
func GetStripePaymentAuthentication(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	id, err := uuid.Parse(r.Param("id"))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid operation id")
		return
	}
	if r.State == nil || r.State.CheckoutService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "payment authentication unavailable")
		return
	}
	resolver, ok := r.State.CollectionResolver.(intents.StripeEngineServiceResolver)
	if !ok {
		r.ErrorJSON(http.StatusServiceUnavailable, "payment authentication unavailable")
		return
	}
	result, err := r.State.CheckoutService.StripePaymentAuthentication(r.Request.Context(), id, checkoutVerifiedPrincipal(r), resolver)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(result)
}
func ConfirmStripePaymentAuthentication(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	id, err := uuid.Parse(r.Param("id"))
	if err != nil || id == uuid.Nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid operation id")
		return
	}
	if r.State == nil || r.State.CheckoutService == nil {
		r.ErrorJSON(http.StatusServiceUnavailable, "payment authentication unavailable")
		return
	}
	result, err := r.State.CheckoutService.ConfirmStripePaymentAuthentication(r.Request.Context(), id, checkoutVerifiedPrincipal(r))
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(result)
}
