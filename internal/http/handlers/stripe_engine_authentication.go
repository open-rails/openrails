package handlers

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/intents"
)

// GetStripePaymentAuthentication answers a payment operation's client secret
// to its interactive payer only; a merchant credential cannot acquire one.
func GetStripePaymentAuthentication(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	operation, err := billing.ParsePaymentOperationID(r.Param("id"))
	if err != nil || operation.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid payment operation id").WithParam("id"))
		return
	}
	id := operation.UUID()
	if r.State == nil || r.State.CheckoutService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "payment authentication unavailable")
		return
	}
	resolver, ok := r.State.CollectionResolver.(intents.StripeEngineServiceResolver)
	if !ok {
		r.ErrorCode(billing.CodeServiceUnavailable, "payment authentication unavailable")
		return
	}
	result, err := r.State.CheckoutService.StripePaymentAuthentication(r.Request.Context(), id, checkoutVerifiedPrincipal(r), resolver)
	if err != nil {
		writeCheckoutAttemptError(r, err)
		return
	}
	r.SuccessJSON(result)
}
func ConfirmStripePaymentAuthentication(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	operation, err := billing.ParsePaymentOperationID(r.Param("id"))
	if err != nil || operation.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid payment operation id").WithParam("id"))
		return
	}
	id := operation.UUID()
	if r.State == nil || r.State.CheckoutService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "payment authentication unavailable")
		return
	}
	result, err := r.State.CheckoutService.ConfirmStripePaymentAuthentication(r.Request.Context(), id, checkoutVerifiedPrincipal(r))
	if err != nil {
		writeCheckoutAttemptError(r, err)
		return
	}
	r.SuccessJSON(result)
}
