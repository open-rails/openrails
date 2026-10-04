package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
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
	if r.State.CheckoutAttemptService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout attempt service unavailable")
		return
	}
	typedParsedID, err := billing.ParseCheckoutAttemptID(sessionID)
	if err != nil || typedParsedID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout attempt id")
		return
	}
	parsedID := typedParsedID.UUID()
	session, err := r.State.CheckoutAttemptService.GetSessionForSolanaPay(r.Request.Context(), parsedID)
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
	if r.State.CheckoutAttemptService == nil {
		r.ErrorJSON(http.StatusInternalServerError, "checkout attempt service unavailable")
		return
	}
	typedParsedID, err := billing.ParseCheckoutAttemptID(sessionID)
	if err != nil || typedParsedID.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout attempt id")
		return
	}
	parsedID := typedParsedID.UUID()
	resp, err := r.State.CheckoutAttemptService.BuildSolanaPayTransaction(r.Request.Context(), parsedID, req.Account)
	if err != nil {
		writeSolanaPayError(r, err)
		return
	}
	r.SuccessJSON(&SolanaPayPostResponse{Transaction: resp.TransactionBase64, Message: resp.Message})
}

func writeSolanaPayError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, checkout.ErrCheckoutAttemptNotFound):
		r.ErrorJSON(http.StatusNotFound, "checkout attempt not found")
	case errors.Is(err, checkout.ErrCheckoutAttemptExpired):
		r.ErrorJSON(http.StatusGone, "checkout attempt expired")
	case errors.Is(err, checkout.ErrCheckoutAttemptNotSolana):
		r.ErrorJSON(http.StatusBadRequest, "not a solana checkout attempt")
	case errors.Is(err, checkout.ErrCheckoutAttemptAlreadyCompleted):
		r.ErrorJSON(http.StatusConflict, "checkout attempt already completed")
	case errors.Is(err, checkout.ErrCheckoutAttemptConflict):
		r.ErrorJSON(http.StatusConflict, "a payment for this checkout is already in progress")
	default:
		r.ErrorJSON(http.StatusInternalServerError, "failed to process request")
	}
}
