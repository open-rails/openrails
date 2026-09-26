package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

type SolanaPayGetResponse struct {
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

type SolanaPayPostRequest struct {
	Account string `json:"account" binding:"required"`
}

type SolanaPayPostResponse struct {
	Transaction string `json:"transaction"`
	Message     string `json:"message,omitempty"`
}

func GetSolanaPay(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.Param("id"))
	if sessionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "id is required")
		return
	}
	if r.State.CheckoutSessionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout session service unavailable")
		return
	}
	typedParsedID, err := openrails.ParseCheckoutSessionID(sessionID)
	if err != nil || typedParsedID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout session id")
		return
	}
	parsedID := typedParsedID.UUID()
	session, err := r.State.CheckoutSessionService.GetSessionForSolanaPay(r.Request.Context(), parsedID)
	if err != nil {
		writeSolanaPayError(r, err)
		return
	}
	label := "Payment"
	icon := ""
	if r.State.DB != nil {
		if cfg, _, err := merchantconfig.NewStore(r.State.DB).Get(r.Request.Context()); err == nil {
			if displayName := strings.TrimSpace(cfg.Profile.DisplayName); displayName != "" {
				label = displayName
			}
			icon = strings.TrimSpace(cfg.Profile.LogoURL)
		}
	}
	if session.ProductName != "" {
		label = session.ProductName
	}
	r.SuccessJSON(&SolanaPayGetResponse{Label: label, Icon: icon})
}

func PostSolanaPay(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.Param("id"))
	if sessionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "id is required")
		return
	}
	var req SolanaPayPostRequest
	if !r.BindJSON(&req) {
		return
	}
	if strings.TrimSpace(req.Account) == "" {
		r.ErrorJSON(http.StatusBadRequest, "account is required")
		return
	}
	if r.State.CheckoutSessionService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout session service unavailable")
		return
	}
	typedParsedID, err := openrails.ParseCheckoutSessionID(sessionID)
	if err != nil || typedParsedID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout session id")
		return
	}
	parsedID := typedParsedID.UUID()
	resp, err := r.State.CheckoutSessionService.BuildSolanaPayTransaction(r.Request.Context(), parsedID, req.Account)
	if err != nil {
		writeSolanaPayError(r, err)
		return
	}
	r.SuccessJSON(&SolanaPayPostResponse{Transaction: resp.TransactionBase64, Message: resp.Message})
}

func writeSolanaPayError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, checkout.ErrCheckoutSessionNotFound):
		r.ErrorJSON(http.StatusNotFound, "checkout session not found")
	case errors.Is(err, checkout.ErrCheckoutSessionExpired):
		r.ErrorJSON(http.StatusGone, "checkout session expired")
	case errors.Is(err, checkout.ErrCheckoutSessionNotSolana):
		r.ErrorJSON(http.StatusBadRequest, "not a solana checkout session")
	case errors.Is(err, checkout.ErrCheckoutSessionAlreadyCompleted):
		r.ErrorJSON(http.StatusConflict, "checkout session already completed")
	case errors.Is(err, checkout.ErrCheckoutSessionConflict):
		r.ErrorJSON(http.StatusConflict, "a payment for this checkout is already in progress")
	default:
		r.ErrorJSON(http.StatusInternalServerError, "failed to process request")
	}
}
