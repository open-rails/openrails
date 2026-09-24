package handlers

import (
	"encoding/json"
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
	r.SetHeader("Cache-Control", "no-store")
	var input struct {
		openrails.CreateCheckoutSessionRequest
		Mode           json.RawMessage `json:"mode"`
		SubscriptionID json.RawMessage `json:"subscription_id"`
		NewPriceID     json.RawMessage `json:"new_price_id"`
	}
	if !r.BindJSON(&input) {
		return
	}
	if raw := input.PaymentOptions.PaymentMethodID; raw != "" {
		id, err := openrails.ParsePaymentMethodID(raw)
		if err != nil || id.IsZero() {
			r.ErrorJSON(http.StatusBadRequest, "invalid payment_method_id")
			return
		}
	}
	if len(input.Mode) > 0 || len(input.SubscriptionID) > 0 || len(input.NewPriceID) > 0 {
		r.ErrorJSON(http.StatusBadRequest, "priced checkout derives its operation from the price; use a dedicated setup or subscription action endpoint")
		return
	}
	payer, ok := commerceCustomer(r, customerIDParam(input.Customer.ID))
	if !ok {
		return
	}
	input.Customer.ID = payer.String()
	input.IdempotencyKey = r.Header("Idempotency-Key")
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		r.ErrorJSON(http.StatusBadRequest, "Idempotency-Key required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.CreateCheckoutSessionForCustomer(r.Request.Context(), input.Customer, input.CreateCheckoutSessionRequest)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{Rail: input.PaymentOptions.Rail, Wallet: input.PaymentOptions.Wallet})
		return
	}
	r.SuccessJSON(out)
}

func ServiceLookupCheckoutSession(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	var input openrails.CreateCheckoutSessionRequest
	if !r.BindJSON(&input) {
		return
	}
	payer, ok := commerceCustomer(r, customerIDParam(input.Customer.ID))
	if !ok {
		return
	}
	input.Customer.ID = payer.String()
	input.IdempotencyKey = r.Header("Idempotency-Key")
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		r.ErrorJSON(http.StatusBadRequest, "Idempotency-Key required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.LookupCheckoutSessionForCustomer(r.Request.Context(), input.Customer, input)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(out)
}

func ServiceGetCheckoutSessionByKey(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	payer, ok := commerceCustomer(r, customerIDParam(r.Query("customer_id")))
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.GetCheckoutSessionByKey(r.Request.Context(), payer.String(), r.Header("Idempotency-Key"), r.Query("entitlement"))
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(out)
}

func ServiceGetCheckoutSession(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
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
	r.SetHeader("Cache-Control", "no-store")
	var input openrails.ConfirmCheckoutSessionRequest
	if !r.BindJSON(&input) {
		return
	}
	payer, ok := commerceCustomer(r, customerIDParam(input.CustomerID))
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
	key := r.Query("price_key")
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.ListCheckoutRailOptions(r.Request.Context(), price, key)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	if len(out) > 0 {
		config, ok := checkoutConfig(r)
		if !ok {
			return
		}
		advertiseCheckoutOptions(out, config)
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

func ServiceCreatePaymentMethodSession(r *httprequest.Request) {
	var input openrails.CreatePaymentMethodSessionRequest
	if !r.BindJSON(&input) {
		return
	}
	input.IdempotencyKey = r.Header("Idempotency-Key")
	serviceCreateCheckoutAction(r, input.Customer, input.IdempotencyKey, input.PaymentOptions, func(svc *billingservice.Service) (*openrails.CheckoutSession, error) {
		return svc.CreatePaymentMethodSessionForCustomer(r.Request.Context(), input)
	})
}

func ServiceCreateSolanaCancelSession(r *httprequest.Request) {
	var input openrails.CreateSolanaCancelSessionRequest
	if !r.BindJSON(&input) {
		return
	}
	input.IdempotencyKey = r.Header("Idempotency-Key")
	serviceCreateCheckoutAction(r, input.Customer, input.IdempotencyKey, input.PaymentOptions, func(svc *billingservice.Service) (*openrails.CheckoutSession, error) {
		return svc.CreateSolanaCancelSessionForCustomer(r.Request.Context(), input)
	})
}

func ServiceCreateSolanaTierChangeSession(r *httprequest.Request) {
	var input openrails.CreateSolanaTierChangeSessionRequest
	if !r.BindJSON(&input) {
		return
	}
	input.IdempotencyKey = r.Header("Idempotency-Key")
	serviceCreateCheckoutAction(r, input.Customer, input.IdempotencyKey, input.PaymentOptions, func(svc *billingservice.Service) (*openrails.CheckoutSession, error) {
		return svc.CreateSolanaTierChangeSessionForCustomer(r.Request.Context(), input)
	})
}

func serviceCreateCheckoutAction(r *httprequest.Request, customer openrails.CheckoutCustomerIdentity, key string, payment openrails.CheckoutPaymentOptions, create func(*billingservice.Service) (*openrails.CheckoutSession, error)) {
	r.SetHeader("Cache-Control", "no-store")
	if _, ok := commerceCustomer(r, customerIDParam(customer.ID)); !ok {
		return
	}
	if strings.TrimSpace(key) == "" {
		r.ErrorJSON(http.StatusBadRequest, "Idempotency-Key required")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	result, err := create(svc)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{Rail: payment.Rail, Wallet: payment.Wallet})
		return
	}
	r.SuccessJSON(result)
}
