package handlers

import (
	"errors"
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/pkg/api"
)

type ChangeTierRequest struct {
	PriceID string `json:"price_id" binding:"required"`
}

func ChangeTier(r *httprequest.Request) {
	var req ChangeTierRequest
	if !r.BindJSON(&req) {
		return
	}

	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}

	subscriptionIDStr := r.GinCtx.Param("id")
	if subscriptionIDStr == "" {
		r.ErrorJSON(http.StatusBadRequest, "subscription ID required")
		return
	}

	subscriptionID, err := api.ParseSubscriptionID(subscriptionIDStr)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "Invalid subscription ID format")
		return
	}

	if r.State.CheckoutService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout service unavailable")
		return
	}

	idempotencyKey := r.GinCtx.GetHeader("X-Idempotency-Key")

	svcReq := &subscriptions.TierChangeRequest{
		PriceID:        req.PriceID,
		SubscriptionID: subscriptionID,
		IdempotencyKey: idempotencyKey,
	}

	resp, err := r.State.CheckoutService.TierChange(r.Request.Context(), svcReq, user)
	if err != nil {
		writeChangeTierError(r, err)
		return
	}

	r.SuccessJSON(resp)
}

func writeChangeTierError(r *httprequest.Request, err error) {
	var tierErr *subscriptions.TierChangeError
	if errors.As(err, &tierErr) {
		r.ErrorJSON(tierErr.HTTPStatus, tierErr.Message)
		return
	}

	var vaultErr *vault.VaultError
	if errors.As(err, &vaultErr) {
		code := api.CodePaymentFailed
		if strings.TrimSpace(vaultErr.LocalizationID) != "" {
			code = vaultErr.LocalizationID
		}
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, code, vaultErr.Error()))
		return
	}

	switch {
	case errors.Is(err, subscriptions.ErrTierChangeNoSubscription):
		r.ErrorJSON(http.StatusNotFound, "no active subscription found")
	case errors.Is(err, subscriptions.ErrTierChangeNotSupported):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	case errors.Is(err, subscriptions.ErrTierChangeBlocked):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, subscriptions.ErrTierChangePending):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, subscriptions.ErrTierChangeSameProduct):
		r.ErrorJSON(http.StatusConflict, "already on this plan")
	case errors.Is(err, subscriptions.ErrTierChangeDifferentGroup):
		r.ErrorJSON(http.StatusBadRequest, "cannot change to a different tier group")
	default:
		r.ErrorJSON(http.StatusInternalServerError, "tier change request failed")
	}
}
