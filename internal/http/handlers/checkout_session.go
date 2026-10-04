package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Checkout sessions (#1124). Minting needs the signed-in customer or the
// merchant; reading and paying need only the session id, which is never
// logged.

// CheckoutSessionMintRequest is the signed-in customer's mint body.
type CheckoutSessionMintRequest struct {
	PriceID    billing.PriceID `json:"price_id"`
	PriceKey   string          `json:"price_key"`
	SuccessURL string          `json:"success_url" binding:"omitempty,url"`
}

// CreateCheckoutSession handles POST /v1/me/checkout-sessions.
func CreateCheckoutSession(r *httprequest.Request) {
	var body CheckoutSessionMintRequest
	if !r.BindJSON(&body) {
		return
	}
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}
	// The id pays with the customer's saved cards: only the customer mints one.
	if !customerInitiatedChargeAllowed(r) {
		return
	}
	customerID, err := billing.ParseCustomerID(user.ID)
	if err != nil || customerID.IsZero() {
		r.ErrorCode(billing.CodeAuthenticationRequired, "")
		return
	}
	customer := billing.CheckoutCustomerIdentity{ID: customerID, Username: user.Username}
	if user.Email != nil {
		customer.VerifiedEmail = *user.Email
	}
	mintCheckoutSession(r, billing.CreateCheckoutSessionRequest{Customer: customer, PriceID: body.PriceID, PriceKey: body.PriceKey, SuccessURL: body.SuccessURL})
}

// ServiceCreateCheckoutSession handles POST /v1/merchant/checkout-sessions:
// the merchant hands a purchase to its customer.
func ServiceCreateCheckoutSession(r *httprequest.Request) {
	var body billing.CreateCheckoutSessionRequest
	if !r.BindJSON(&body) {
		return
	}
	if _, ok := commerceCustomer(r, body.Customer.ID); !ok {
		return
	}
	mintCheckoutSession(r, body)
}

func mintCheckoutSession(r *httprequest.Request, req billing.CreateCheckoutSessionRequest) {
	r.SetHeader("Cache-Control", "no-store")
	config, ok := checkoutConfig(r)
	if !ok {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	link, err := svc.CreateCheckoutSession(r.Request.Context(), billingservice.CheckoutSessionMint{
		CreateCheckoutSessionRequest: req,
		Advertise:                    func(options []billing.CheckoutOption) { advertiseCheckoutOptions(options, config) },
	})
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return
	}
	r.JSON(http.StatusCreated, link)
}

// GetCheckoutSession handles GET /v1/checkout-sessions/{id}.
func GetCheckoutSession(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	session, err := svc.GetCheckoutSession(r.Request.Context(), r.Param("id"))
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return
	}
	r.SuccessJSON(session)
}

// PayCheckoutSession handles POST /v1/checkout-sessions/{id}/pay.
func PayCheckoutSession(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	var body checkoutsession.CheckoutSessionPayRequest
	if !r.BindJSON(&body) {
		return
	}
	defer body.Card.Zero()
	// Every other field is scanned by the engine's checkout.
	if body.Card != nil && !cardFieldAdmitted(r, strings.TrimSpace(body.PaymentToken) != "") {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	result, err := svc.PayCheckoutSession(r.Request.Context(), r.Param("id"), body, r.ClientIP())
	if err != nil {
		writeCheckoutAttemptError(r, err, checkoutAttemptErrorContext{})
		return
	}
	if result.Status == "failed" && result.Failure != nil {
		recordCardFailure(r)
	}
	r.SuccessJSON(result)
}
