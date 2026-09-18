package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// commerceCustomer resolves a customer id from a request body field or a
// path/query string and enforces the caller's customer scope.
func commerceCustomer(r *httprequest.Request, customerID openrails.CustomerID) (identity.CustomerID, bool) {
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

func ServiceCreateCheckoutSession(r *httprequest.Request) {
	var input openrails.CreateCheckoutSessionRequest
	if !r.BindJSON(&input) {
		return
	}
	payer, ok := commerceCustomer(r, input.Customer.ID)
	if !ok {
		return
	}
	input.Customer.ID = openrails.CustomerID(payer)
	input.IdempotencyKey = strings.TrimSpace(r.Header("Idempotency-Key"))
	if input.IdempotencyKey == "" {
		r.ErrorJSON(http.StatusBadRequest, "Idempotency-Key required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.CreateCheckoutSessionForCustomer(r.Request.Context(), input.Customer, input)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{Rail: input.Payment.Rail, Wallet: input.Payment.Wallet})
		return
	}
	r.SuccessJSON(out)
}

func ServiceGetCheckoutSession(r *httprequest.Request) {
	payer, ok := commerceCustomer(r, customerIDParam(r.Query("customer_id")))
	if !ok {
		return
	}
	typedId, err := openrails.ParseCheckoutSessionID(r.Param("id"))
	if err != nil || typedId.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout session id")
		return
	}
	id := typedId.UUID()
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.GetCheckoutSession(r.Request.Context(), payer.String(), id)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(out)
}

func ServiceConfirmCheckoutSession(r *httprequest.Request) {
	var input openrails.ConfirmCheckoutSessionRequest
	if !r.BindJSON(&input) {
		return
	}
	payer, ok := commerceCustomer(r, input.CustomerID)
	if !ok {
		return
	}
	typedId, err := openrails.ParseCheckoutSessionID(r.Param("id"))
	if err != nil || typedId.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout session id")
		return
	}
	id := typedId.UUID()
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.ConfirmCheckoutSession(r.Request.Context(), payer.String(), id, input)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{CheckoutSessionID: id.String(), Rail: input.Payment.Rail, Wallet: input.Payment.Wallet})
		return
	}
	r.SuccessJSON(out)
}

func ServiceListCheckoutRailOptions(r *httprequest.Request) {
	price := strings.TrimSpace(r.Query("price_id"))
	if price == "" {
		r.ErrorJSON(http.StatusBadRequest, "price_id required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.ListCheckoutRailOptions(r.Request.Context(), price)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(out)
}

func ServiceResolveEffectiveTier(r *httprequest.Request) {
	payer, ok := commerceCustomer(r, customerIDParam(r.Param("customer_id")))
	if !ok {
		return
	}
	group := strings.TrimSpace(r.Query("group"))
	if group == "" {
		r.ErrorJSON(http.StatusBadRequest, "group required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.ResolveEffectiveTier(r.Request.Context(), payer.String(), group)
	if err != nil {
		r.InternalError("effective tier lookup failed", err)
		return
	}
	r.SuccessJSON(out)
}
