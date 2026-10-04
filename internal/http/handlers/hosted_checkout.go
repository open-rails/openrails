package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/internal/service"
)

// Hosted checkout (#1124). Minting needs the signed-in customer; reading and
// paying need only the session id, which is never logged.

type hostedCheckoutMintRequest struct {
	PriceID    string `json:"price_id,omitempty"`
	PriceKey   string `json:"price_key,omitempty"`
	SuccessURL string `json:"success_url,omitempty" binding:"omitempty,url"`
}

// CreateHostedCheckoutSession handles POST /v1/me/checkout/sessions.
func CreateHostedCheckoutSession(r *httprequest.Request) {
	var body hostedCheckoutMintRequest
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
	customer := billing.CheckoutCustomerIdentity{ID: user.ID, Username: user.Username}
	if user.Email != nil {
		customer.VerifiedEmail = *user.Email
	}
	mintHostedCheckoutSession(r, billing.CreateHostedCheckoutSessionRequest{Customer: customer, PriceID: body.PriceID, PriceKey: body.PriceKey, SuccessURL: body.SuccessURL})
}

// ServiceCreateHostedCheckoutSession handles POST
// /v1/merchant/hosted-checkout-sessions for a host that mints server-side.
func ServiceCreateHostedCheckoutSession(r *httprequest.Request) {
	var body billing.CreateHostedCheckoutSessionRequest
	if !r.BindJSON(&body) {
		return
	}
	payer, ok := commerceCustomer(r, customerIDParam(body.Customer.ID))
	if !ok {
		return
	}
	body.Customer.ID = payer.String()
	mintHostedCheckoutSession(r, body)
}

func mintHostedCheckoutSession(r *httprequest.Request, req billing.CreateHostedCheckoutSessionRequest) {
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
	link, err := svc.CreateHostedCheckoutSession(r.Request.Context(), billingservice.HostedCheckoutMint{
		CreateHostedCheckoutSessionRequest: req,
		Advertise:                          func(options []billing.CheckoutRailOption) { advertiseCheckoutOptions(options, config) },
	})
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.JSON(http.StatusCreated, link)
}

// GetHostedCheckoutSession handles GET /v1/checkout-sessions/{id}.
func GetHostedCheckoutSession(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	session, err := svc.GetHostedCheckoutSession(r.Request.Context(), r.Param("id"))
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	r.SuccessJSON(session)
}

// PayHostedCheckoutSession handles POST /v1/checkout-sessions/{id}/pay.
func PayHostedCheckoutSession(r *httprequest.Request) {
	r.SetHeader("Cache-Control", "no-store")
	var body billing.HostedCheckoutPayRequest
	if !r.BindJSON(&body) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	result, err := svc.PayHostedCheckoutSession(r.Request.Context(), r.Param("id"), body, r.ClientIP())
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		return
	}
	if result.Status == "failed" && result.Failure != nil {
		recordCardFailure(r)
	}
	r.SuccessJSON(result)
}
