package handlers

import (
	"errors"
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

// CreateCheckoutSession handles POST /v1/checkout
func CreateCheckoutSession(r *httprequest.Request) {
	var req CheckoutSessionCreateRequest
	if !r.BindJSON(&req) {
		return
	}

	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}
	if r.State.CheckoutSessionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout session service unavailable")
		return
	}

	req.IdempotencyKey = r.GinCtx.GetHeader("X-Idempotency-Key")
	e2eRunID := strings.TrimSpace(r.GinCtx.GetHeader("X-E2E-Run-ID"))
	if e2eRunID != "" {
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		if _, ok := req.Metadata["e2e_run_id"]; !ok {
			req.Metadata["e2e_run_id"] = e2eRunID
		}
	}
	svcReq := &services.CheckoutSessionCreateRequest{
		PriceID:        req.PriceID,
		Mode:           req.Mode,
		Metadata:       req.Metadata,
		IdempotencyKey: req.IdempotencyKey,
		Payment: services.CheckoutSessionPaymentRequest{
			Processor:       req.Payment.Processor,
			PaymentMethodID: req.Payment.PaymentMethodID,
			PaymentToken:    req.Payment.PaymentToken,
			TokenSymbol:     req.Payment.TokenSymbol,
			Flow:            req.Payment.Flow,
			Wallet:          req.Payment.Wallet,
			Email:           req.Payment.Email,
			FirstName:       req.Payment.FirstName,
			LastName:        req.Payment.LastName,
			Address1:        req.Payment.Address1,
			City:            req.Payment.City,
			State:           req.Payment.State,
			Zip:             req.Payment.Zip,
			Country:         req.Payment.Country,
			LastFour:        req.Payment.LastFour,
			CardType:        req.Payment.CardType,
			ExpiryDate:      req.Payment.ExpiryDate,
		},
	}

	resp, err := r.State.CheckoutSessionService.CreateSession(r.Request.Context(), svcReq, user)
	if err != nil {
		log.WithError(err).Error("Failed to create checkout session")
		writeCheckoutSessionError(r, err)
		return
	}

	r.SuccessJSON(resp)
}

// GetCheckoutSession handles GET /v1/checkout/:id
func GetCheckoutSession(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.GinCtx.Param("id"))
	if sessionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "id is required")
		return
	}

	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}
	if r.State.CheckoutSessionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout session service unavailable")
		return
	}

	parsedID, err := api.ParseCheckoutSessionID(sessionID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout session id")
		return
	}

	resp, err := r.State.CheckoutSessionService.GetSession(r.Request.Context(), parsedID, user)
	if err != nil {
		writeCheckoutSessionError(r, err)
		return
	}

	r.SuccessJSON(resp)
}

// ConfirmCheckoutSession handles POST /v1/checkout/:id/confirm
func ConfirmCheckoutSession(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.GinCtx.Param("id"))
	if sessionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "id is required")
		return
	}

	var req CheckoutSessionConfirmRequest
	if !r.BindJSON(&req) {
		return
	}

	user := r.GetUser()
	if user == nil || strings.TrimSpace(user.ID) == "" {
		r.ErrorJSON(http.StatusUnauthorized, "authentication required")
		return
	}
	if r.State.CheckoutSessionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout session service unavailable")
		return
	}

	parsedID, err := api.ParseCheckoutSessionID(sessionID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout session id")
		return
	}

	svcReq := &services.CheckoutSessionConfirmRequest{
		Payment: services.CheckoutSessionConfirmPayment{
			Processor: req.Payment.Processor,
			Signature: req.Payment.Signature,
			Wallet:    req.Payment.Wallet,
		},
	}

	resp, err := r.State.CheckoutSessionService.ConfirmSession(r.Request.Context(), parsedID, svcReq, user)
	if err != nil {
		writeCheckoutSessionError(r, err)
		return
	}

	r.SuccessJSON(resp)
}

func writeCheckoutSessionError(r *httprequest.Request, err error) {
	var vaultErr *services.VaultError
	if errors.As(err, &vaultErr) {
		code := api.CodePaymentFailed
		if strings.TrimSpace(vaultErr.LocalizationID) != "" {
			code = vaultErr.LocalizationID
		}
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, code, vaultErr.Error()))
		return
	}

	switch {
	case errors.Is(err, services.ErrCheckoutSessionNotFound):
		r.ErrorJSON(http.StatusNotFound, err.Error())
	case errors.Is(err, services.ErrCheckoutSessionForbidden):
		r.ErrorJSON(http.StatusForbidden, err.Error())
	case errors.Is(err, services.ErrCheckoutSessionExpired):
		r.ErrorJSON(http.StatusGone, err.Error())
	case errors.Is(err, services.ErrCheckoutSessionPending):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, services.ErrCheckoutSessionConflict):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, services.ErrCheckoutSessionValidation):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		r.ErrorJSON(http.StatusInternalServerError, "checkout session request failed")
	}
}
