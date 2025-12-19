package handlers

import (
	"net/http"
	"net/url"
	"strings"

	log "github.com/sirupsen/logrus"

	authgin "github.com/PaulFidika/authkit/adapters/gin"
	"github.com/doujins-org/doujins-billing/internal/services"
)

// Checkout handles unified checkout for both subscriptions and one-time purchases
// POST /v1/me/checkout
//
// Request body:
//
//	{
//	  "price_id": "uuid",            // Required: Price to purchase
//	  "processor": "mobius|ccbill|solana", // Required: Payment processor
//	  "payment_method_id": "uuid",   // Optional: Stored payment method (for mobius)
//	  "payment_token": "string",     // Optional: Collect.js token (for mobius)
//	  "email": "string",             // Optional: Email for billing
//	  "first_name": "string",        // Optional: First name for billing
//	  "last_name": "string",         // Optional: Last name for billing
//	  "address1": "string",          // Optional: Address for billing
//	  "city": "string",              // Optional: City for billing
//	  "state": "string",             // Optional: State for billing
//	  "zip": "string",               // Optional: Zip for billing
//	  "country": "string"            // Optional: Country code (2 letters)
//	}
//
// Response:
//
//	{
//	  "status": "success|pending|redirect_required|blocked",
//	  "message": "string",
//	  "subscription_id": "uuid",     // For subscription purchases
//	  "payment_id": "uuid",          // For one-time purchases
//	  "transaction_id": "string",    // Processor transaction ID
//	  "redirect_url": "string",      // For CCBill FlexForm
//	  "delayed_start": "datetime"    // When purchase takes effect (if delayed)
//	}
//
// Behavior:
//   - If price.billing_cycle_days != nil → subscription flow
//   - If price.billing_cycle_days == nil → one-time purchase flow
//   - Checks for existing coverage (subscriptions + entitlements)
//   - If user has indefinite coverage → blocks purchase
//   - If user has coverage with end date → allows with delayed start
//   - CCBill: cannot delay start, blocks if user has existing coverage
//   - NMI/mobius: creates subscription with start_date = coverage_end_date
//   - Solana: charges immediately, delays entitlement start to coverage_end_date
func Checkout(r *Request) {
	var req CheckoutRequest
	if !r.BindJSON(&req) {
		return
	}

	cl, ok := authgin.ClaimsFromGin(r.GinCtx)
	if !ok || cl.UserID == "" {
		r.ErrorJSON(http.StatusUnauthorized, "User authentication required")
		return
	}

	if r.State.CheckoutService == nil {
		log.Error("Checkout service is not configured")
		r.ErrorJSON(http.StatusInternalServerError, "Checkout service unavailable")
		return
	}

	// Convert handler request to service request
	checkoutReq := &services.CheckoutRequest{
		PriceID:         req.PriceID,
		PaymentMethodID: req.PaymentMethodID,
		PaymentToken:    req.PaymentToken,
		Processor:       req.Processor,
		Email:           req.Email,
		FirstName:       req.FirstName,
		LastName:        req.LastName,
		Address1:        req.Address1,
		City:            req.City,
		State:           req.State,
		Zip:             req.Zip,
		Country:         req.Country,
		IdempotencyKey:  r.GinCtx.GetHeader("Idempotency-Key"),
	}
	baseURL := guessBaseURL(r.Request)
	if baseURL != "" {
		checkoutReq.SuccessURL = baseURL + "/account?subscription=success"
		checkoutReq.CancelURL = baseURL + "/account?subscription=canceled"
	}

	user := &services.UserIdentity{
		ID:       cl.UserID,
		Username: cl.Username,
		Roles:    cl.Roles,
	}
	if cl.Email != "" {
		email := cl.Email
		user.Email = &email
	}

	resp, err := r.State.CheckoutService.Checkout(r.Request.Context(), checkoutReq, user)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"user_id":   cl.UserID,
			"price_id":  req.PriceID,
			"processor": req.Processor,
		}).Error("Checkout failed")
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}

	// Determine HTTP status based on response status
	httpStatus := http.StatusOK
	if resp.Status == "blocked" {
		httpStatus = http.StatusConflict
	}

	r.GinCtx.JSON(httpStatus, resp)
}

func guessBaseURL(req *http.Request) string {
	if req == nil {
		return ""
	}
	if origin := strings.TrimSpace(req.Header.Get("Origin")); origin != "" {
		if u, err := url.Parse(origin); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	if ref := strings.TrimSpace(req.Header.Get("Referer")); ref != "" {
		if u, err := url.Parse(ref); err == nil && u.Scheme != "" && u.Host != "" {
			return u.Scheme + "://" + u.Host
		}
	}
	scheme := strings.TrimSpace(req.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if req.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := strings.TrimSpace(req.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = req.Host
	}
	if host == "" || scheme == "" {
		return ""
	}
	return scheme + "://" + host
}
