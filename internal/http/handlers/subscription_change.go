package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ChangeSubscriptionRequest is the staff change and its preview.
type ChangeSubscriptionRequest = billing.ChangeSubscriptionParams

// PreviewSubscriptionChangeParams is the customer's preview of a change.
type PreviewSubscriptionChangeParams struct {
	PriceID  *billing.PriceID `json:"price_id"`
	Quantity *int             `json:"quantity"`
}

// CustomerChangeSubscriptionParams is the customer's change. Signature
// completes a solana_sign_transactions next action: the signature of the
// tier-change transaction the customer's wallet sent.
type CustomerChangeSubscriptionParams struct {
	PriceID   *billing.PriceID `json:"price_id"`
	Quantity  *int             `json:"quantity"`
	Signature string           `json:"signature"`
}

// changeRequest validates a change body and the subscription it names.
func changeRequest(r *httprequest.Request, priceID *billing.PriceID, quantity *int) (*payment.SubscriptionChangeRequest, bool) {
	if priceID == nil && quantity == nil {
		r.APIError(api.Coded(billing.CodeInvalidParam, "a change names a price_id, a quantity or both"))
		return nil, false
	}
	if priceID != nil && priceID.IsZero() {
		r.APIError(api.Coded(billing.CodeInvalidParam, "invalid price_id").WithParam("price_id"))
		return nil, false
	}
	if quantity != nil && *quantity < 1 {
		r.APIError(api.Coded(billing.CodeInvalidParam, "quantity must be at least 1").WithParam("quantity"))
		return nil, false
	}
	id, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid subscription ID")
		return nil, false
	}
	if r.State.PurchaseService == nil {
		r.ErrorCode(billing.CodeInternalError, "purchase service unavailable")
		return nil, false
	}
	req := &payment.SubscriptionChangeRequest{SubscriptionID: id.UUID(), Quantity: quantity}
	if priceID != nil {
		req.PriceID = priceID.String()
	}
	return req, true
}

// ChangeSubscription changes one of the customer's subscriptions: another
// price of its tier group, other seats, or both. More seats and an upgrade
// charge now; fewer seats and a downgrade apply at the next renewal; a rail
// that needs the customer's own step answers requires_action with
// next_action.
func ChangeSubscription(r *httprequest.Request) {
	var body CustomerChangeSubscriptionParams
	if !r.BindJSON(&body) {
		return
	}
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "authentication required")
		return
	}
	req, ok := changeRequest(r, body.PriceID, body.Quantity)
	if !ok {
		return
	}
	// A change may charge the saved card now.
	if !customerInitiatedChargeAllowed(r) {
		return
	}
	// The wallet signs a Solana tier change; ownership is checked there.
	if sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), req.SubscriptionID); err == nil && sub.Rail == models.RailSolana {
		if body.Quantity != nil {
			r.APIError(api.Coded(billing.CodeQuantityNotAllowed, "a Solana subscription has no seats").WithParam("quantity"))
			return
		}
		solanaTierChange(r, req.SubscriptionID, req.PriceID, strings.TrimSpace(body.Signature))
		return
	}
	if body.Signature != "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "signature completes a wallet step; this subscription's rail has none").WithParam("signature"))
		return
	}
	req.IdempotencyKey = strings.TrimSpace(r.Header("Idempotency-Key"))
	payer := verifiedPrincipal(r)
	req.Payer = &payer
	r.SetHeader("Cache-Control", "no-store") // next_action may carry a client secret
	resp, err := r.State.PurchaseService.ChangeSubscription(r.Request.Context(), req, user)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}
	writeTierChangeResponse(r, resp)
}

// ConfirmSubscriptionChange reads the provider for the customer's change
// awaiting them, after they completed its next_action, and answers the change.
func ConfirmSubscriptionChange(r *httprequest.Request) {
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "authentication required")
		return
	}
	id, err := billing.ParseSubscriptionID(r.Param("id"))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid subscription ID")
		return
	}
	if r.State.PurchaseService == nil {
		r.ErrorCode(billing.CodeInternalError, "purchase service unavailable")
		return
	}
	r.SetHeader("Cache-Control", "no-store")
	resp, err := r.State.PurchaseService.ConfirmSubscriptionChange(r.Request.Context(), id.UUID(), user, verifiedPrincipal(r))
	if err != nil {
		writeChangeTierError(r, err)
		return
	}
	writeTierChangeResponse(r, resp)
}

// writeTierChangeResponse answers a durable change: 202 while the provider
// outcome is unresolved (the same Idempotency-Key replays the result), 200
// otherwise.
func writeTierChangeResponse(r *httprequest.Request, resp *payment.TierChangeResponse) {
	if resp.Status == "processing" {
		r.JSON(http.StatusAccepted, resp)
		return
	}
	r.SuccessJSON(resp)
}

// PreviewSubscriptionChange is the non-mutating dry-run of ChangeSubscription:
// what it would charge now and at the next renewal.
func PreviewSubscriptionChange(r *httprequest.Request) {
	var body PreviewSubscriptionChangeParams
	if !r.BindJSON(&body) {
		return
	}
	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "authentication required")
		return
	}
	req, ok := changeRequest(r, body.PriceID, body.Quantity)
	if !ok {
		return
	}
	resp, err := r.State.PurchaseService.PreviewSubscriptionChange(r.Request.Context(), req, user)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}
	r.SuccessJSON(resp)
}

func writeChangeTierError(r *httprequest.Request, err error) {
	var refusal *api.APIError
	if errors.As(err, &refusal) {
		r.APIError(refusal)
		return
	}
	var inFlight *payment.TierChangeInFlightError
	if errors.As(err, &inFlight) {
		r.ErrorCode(billing.CodeSubscriptionChangeInFlight, inFlight.Error())
		return
	}
	var tierErr *payment.TierChangeError
	if errors.As(err, &tierErr) {
		r.ErrorCode(tierErr.Code, tierErr.Message)
		return
	}
	var declined *payment.TierChangeDeclinedError
	if errors.As(err, &declined) {
		r.APIError(railPaymentRefusalError(declined.Rail, declined.FailureCode, declined.Reason))
		return
	}

	var pmErr *paymentmethods.PaymentMethodError
	if errors.As(err, &pmErr) {
		writePaymentMethodError(r, pmErr)
		return
	}
	if errors.Is(err, payment.ErrPaymentMethodStale) {
		writePaymentMethodStale(r)
		return
	}

	switch {
	case errors.Is(err, payment.ErrTierChangeNoSubscription):
		r.ErrorCode(billing.CodeResourceNotFound, "no active subscription found")
	case errors.Is(err, payment.ErrTierChangePending), errors.Is(err, payment.ErrPurchaseProcessing), errors.Is(err, payment.ErrNoChangeToConfirm):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, payment.ErrTierChangeSameProduct):
		r.ErrorCode(billing.CodeResourceConflict, "already on this plan")
	case errors.Is(err, payment.ErrTierChangeDifferentGroup):
		r.ErrorCode(billing.CodeInvalidParam, "cannot change to a different tier group")
	case errors.Is(err, subscriptions.ErrPriceCurrencyMismatch):
		r.ErrorCode(billing.CodeInvalidParam, "cannot change to a plan in a different currency")
	default:
		writeRefusal(r, err, "tier change request failed")
	}
}
