package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"

	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ChangeTierRequest is the merchant's tier change and both previews.
type ChangeTierRequest = billing.ChangeTierParams

// CustomerChangeTierParams is the customer's tier change. Signature
// completes a solana_sign_transactions next action: the signature of the
// tier-change transaction the customer's wallet sent.
type CustomerChangeTierParams struct {
	PriceID   billing.PriceID `json:"price_id"`
	Signature string          `json:"signature"`
}

// ChangeTier moves one of the customer's subscriptions to another price of
// its tier group. An upgrade charges now, a downgrade applies at period end;
// a rail that needs the customer's own step answers requires_action with
// next_action.
func ChangeTier(r *httprequest.Request) {
	var req CustomerChangeTierParams
	if !r.BindJSON(&req) {
		return
	}
	if req.PriceID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid price_id")
		return
	}

	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "authentication required")
		return
	}
	// An upgrade charges the saved card now.
	if !customerInitiatedChargeAllowed(r) {
		return
	}

	subscriptionIDStr := r.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorCode(billing.CodeInvalidParam, "subscription ID required")
		return
	}

	typedSubscriptionID, err := billing.ParseSubscriptionID(subscriptionIDStr)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "Invalid subscription ID format")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()

	if r.State.CheckoutService == nil {
		r.ErrorCode(billing.CodeInternalError, "checkout service unavailable")
		return
	}

	// The wallet signs a Solana tier change; ownership is checked there.
	if sub, err := r.State.SubscriptionService.GetByID(r.Request.Context(), subscriptionID); err == nil && sub.Rail == models.RailSolana {
		solanaTierChange(r, subscriptionID, req.PriceID.String(), strings.TrimSpace(req.Signature))
		return
	}
	if req.Signature != "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "signature completes a wallet step; this subscription's rail has none").WithParam("signature"))
		return
	}

	idempotencyKey := strings.TrimSpace(r.Header("Idempotency-Key"))

	svcReq := &checkout.TierChangeRequest{
		PriceID:        req.PriceID.String(),
		SubscriptionID: subscriptionID,
		IdempotencyKey: idempotencyKey,
	}

	resp, err := r.State.CheckoutService.TierChange(r.Request.Context(), svcReq, user)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}

	writeTierChangeResponse(r, resp)
}

// writeTierChangeResponse answers a durable tier change: 202 while the
// provider outcome is unresolved (the body names the operation; the same
// Idempotency-Key replays the result), 200 otherwise.
func writeTierChangeResponse(r *httprequest.Request, resp *checkout.TierChangeResponse) {
	if resp.Status == "processing" {
		r.JSON(http.StatusAccepted, resp)
		return
	}
	r.SuccessJSON(resp)
}

// ChangeTierPreview is the non-mutating dry-run of ChangeTier: it returns what a
// confirm WOULD charge now and at the next renewal, without touching the
// rail or the local subscription. The frontend renders it as a
// "Right now: $X / On <date>: $Y" confirmation before calling ChangeTier.
func ChangeTierPreview(r *httprequest.Request) {
	var req ChangeTierRequest
	if !r.BindJSON(&req) {
		return
	}
	if req.PriceID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid price_id")
		return
	}

	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorCode(billing.CodeAuthenticationRequired, "authentication required")
		return
	}

	subscriptionIDStr := r.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorCode(billing.CodeInvalidParam, "subscription ID required")
		return
	}

	typedSubscriptionID, err := billing.ParseSubscriptionID(subscriptionIDStr)
	if err != nil || typedSubscriptionID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "Invalid subscription ID format")
		return
	}
	subscriptionID := typedSubscriptionID.UUID()

	if r.State.CheckoutService == nil {
		r.ErrorCode(billing.CodeInternalError, "checkout service unavailable")
		return
	}

	svcReq := &checkout.TierChangeRequest{
		PriceID:        req.PriceID.String(),
		SubscriptionID: subscriptionID,
	}

	resp, err := r.State.CheckoutService.TierChangePreview(r.Request.Context(), svcReq, user)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}

	r.SuccessJSON(resp)
}

func writeChangeTierError(r *httprequest.Request, err error) {
	var inFlight *checkout.TierChangeInFlightError
	if errors.As(err, &inFlight) {
		r.APIError(api.NewAPIError(http.StatusConflict, api.ErrorTypeInvalidRequest, billing.CodeTierChangeInFlight, inFlight.Error()).
			WithMetadata(map[string]any{"operation_id": inFlight.OperationID.String()}))
		return
	}
	var tierErr *checkout.TierChangeError
	if errors.As(err, &tierErr) {
		if tierErr.Code != "" {
			r.APIError(api.NewAPIError(tierErr.HTTPStatus, api.ErrorTypeForStatus(tierErr.HTTPStatus), tierErr.Code, tierErr.Message))
			return
		}
		r.ErrorJSON(tierErr.HTTPStatus, tierErr.Message)
		return
	}

	var pmErr *paymentmethods.PaymentMethodError
	if errors.As(err, &pmErr) {
		writePaymentMethodError(r, pmErr)
		return
	}
	if errors.Is(err, checkout.ErrPaymentMethodStale) {
		writePaymentMethodStale(r)
		return
	}

	switch {
	case errors.Is(err, checkout.ErrTierChangeNoSubscription):
		r.ErrorCode(billing.CodeResourceNotFound, "no active subscription found")
	case errors.Is(err, checkout.ErrTierChangeNotSupported):
		r.ErrorCode(billing.CodeInvalidParam, err.Error())
	case errors.Is(err, checkout.ErrTierChangeBlocked):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, checkout.ErrTierChangePending), errors.Is(err, checkout.ErrCheckoutProcessing):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, checkout.ErrTierChangeSameProduct):
		r.ErrorCode(billing.CodeResourceConflict, "already on this plan")
	case errors.Is(err, checkout.ErrTierChangeDifferentGroup):
		r.ErrorCode(billing.CodeInvalidParam, "cannot change to a different tier group")
	case errors.Is(err, subscriptions.ErrRepriceCrossCurrency):
		r.ErrorCode(billing.CodeInvalidParam, "cannot change to a plan in a different currency")
	default:
		writeRefusal(r, err, "tier change request failed")
	}
}
