package handlers

import (
	"errors"
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
		r.ErrorCode(billing.CodeInvalidParam, "id is required")
		return
	}
	if r.State.CheckoutAttemptService == nil {
		r.ErrorCode(billing.CodeInternalError, "checkout attempt service unavailable")
		return
	}
	typedParsedID, err := billing.ParseCheckoutAttemptID(sessionID)
	if err != nil || typedParsedID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid checkout attempt id")
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
		r.ErrorCode(billing.CodeInvalidParam, "id is required")
		return
	}
	var req SolanaPayPostRequest
	if !r.BindJSON(&req) {
		return
	}
	if strings.TrimSpace(req.Account) == "" {
		r.ErrorCode(billing.CodeInvalidParam, "account is required")
		return
	}
	if r.State.CheckoutAttemptService == nil {
		r.ErrorCode(billing.CodeInternalError, "checkout attempt service unavailable")
		return
	}
	typedParsedID, err := billing.ParseCheckoutAttemptID(sessionID)
	if err != nil || typedParsedID.IsZero() {
		r.ErrorCode(billing.CodeInvalidParam, "invalid checkout attempt id")
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
		r.ErrorCode(billing.CodeResourceNotFound, "checkout attempt not found")
	case errors.Is(err, checkout.ErrCheckoutAttemptExpired):
		r.ErrorCode("checkout_attempt_expired", "")
	case errors.Is(err, checkout.ErrCheckoutAttemptNotSolana):
		r.ErrorCode(billing.CodeInvalidParam, "not a solana checkout attempt")
	case errors.Is(err, checkout.ErrCheckoutAttemptAlreadyCompleted):
		r.ErrorCode(billing.CodeResourceConflict, "checkout attempt already completed")
	case errors.Is(err, checkout.ErrCheckoutAttemptConflict):
		r.ErrorCode(billing.CodeResourceConflict, "a payment for this checkout is already in progress")
	default:
		r.ErrorCode(billing.CodeInternalError, "failed to process request")
	}
}
