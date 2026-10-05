package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// commerceCustomer requires a customer id and the caller's scope over it.
func commerceCustomer(r *httprequest.Request, customerID billing.CustomerID) (identity.CustomerID, bool) {
	id := servicePayer(customerID)
	if id == nil {
		r.ErrorJSON(http.StatusBadRequest, "valid customer_id required")
		return identity.CustomerID{}, false
	}
	if !requireServiceCustomerScope(r, *id) {
		return identity.CustomerID{}, false
	}
	return *id, true
}

// ServiceCreateCheckoutAttempt handles POST /v1/merchant/checkout-attempts.
func ServiceCreateCheckoutAttempt(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	var input billing.CreateCheckoutAttemptParams
	if !r.BindJSON(&input) {
		return
	}
	if _, ok := commerceCustomer(r, input.Customer.ID); !ok {
		return
	}
	input.IdempotencyKey = r.Header("Idempotency-Key")
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		r.ErrorCode("idempotency_key_required", "")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.CreateCheckoutAttempt(r.Request.Context(), input)
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{Rail: input.PaymentOptions.PSP, Wallet: input.PaymentOptions.Wallet})
		return
	}
	r.SuccessJSON(out)
}

// attemptInScope reads the {id} attempt's customer and checks the caller may
// act for it.
func attemptInScope(r *httprequest.Request, svc *billingservice.Service) (billing.CheckoutAttemptID, bool) {
	id, err := billing.ParseCheckoutAttemptID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout attempt id")
		return id, false
	}
	owner, err := svc.CheckoutAttemptOwner(r.Request.Context(), id)
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return id, false
	}
	_, ok := commerceCustomer(r, owner)
	return id, ok
}

// ServiceGetCheckoutAttempt handles GET /v1/merchant/checkout-attempts/{id}.
func ServiceGetCheckoutAttempt(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	id, ok := attemptInScope(r, svc)
	if !ok {
		return
	}
	out, err := svc.GetCheckoutAttempt(r.Request.Context(), id)
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return
	}
	r.SuccessJSON(out)
}

// ServiceConfirmCheckoutAttempt handles POST
// /v1/merchant/checkout-attempts/{id}/confirm.
func ServiceConfirmCheckoutAttempt(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	var input billing.ConfirmCheckoutAttemptParams
	if !r.BindJSON(&input) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	id, ok := attemptInScope(r, svc)
	if !ok {
		return
	}
	out, err := svc.ConfirmCheckoutAttempt(r.Request.Context(), id, input)
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{CheckoutAttemptID: id.String(), Rail: "solana", Wallet: input.Wallet})
		return
	}
	if out.Status == billing.CheckoutAttemptProcessing {
		r.JSON(http.StatusAccepted, out)
		return
	}
	r.SuccessJSON(out)
}
