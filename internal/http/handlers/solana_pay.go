package handlers

import (
	"errors"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/orders"
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

// solanaOrderID is the order a wallet's request names, "" after answering.
func solanaOrderID(r *httprequest.Request) (billing.OrderID, bool) {
	id, err := billing.ParseOrderID(strings.TrimSpace(r.Param("id")))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeResourceNotFound, "order not found")
		return billing.OrderID{}, false
	}
	if r.State.CheckoutAttemptService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "solana pay unavailable")
		return billing.OrderID{}, false
	}
	return id, true
}

// GetOrderSolanaPay handles GET /v1/orders/{id}/solana-pay: what a wallet
// shows before it asks for the order's transaction.
func GetOrderSolanaPay(r *httprequest.Request) {
	id, ok := solanaOrderID(r)
	if !ok {
		return
	}
	items, err := r.State.CheckoutAttemptService.OrderSolanaLabel(r.Request.Context(), id.UUID())
	if err != nil {
		writeOrderSolanaPayError(r, err)
		return
	}
	label, icon := "Payment", ""
	if r.State.DB != nil {
		if cfg, _, err := merchantconfig.NewStore(r.State.DB).Get(r.Request.Context()); err == nil {
			if name := strings.TrimSpace(cfg.Profile.DisplayName); name != "" {
				label = name
			}
			icon = strings.TrimSpace(cfg.Profile.LogoURL)
		}
	}
	if items != "" {
		label = items
	}
	r.SuccessJSON(&SolanaPayGetResponse{Label: label, Icon: icon})
}

// PostOrderSolanaPay handles POST /v1/orders/{id}/solana-pay: the wallet's
// account in, the order's transaction out.
func PostOrderSolanaPay(r *httprequest.Request) {
	var req SolanaPayPostRequest
	if !r.BindJSON(&req) {
		return
	}
	id, ok := solanaOrderID(r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Account) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "account is required").WithParam("account"))
		return
	}
	resp, err := r.State.CheckoutAttemptService.BuildOrderSolanaTransaction(r.Request.Context(), id.UUID(), req.Account)
	if err != nil {
		writeOrderSolanaPayError(r, err)
		return
	}
	r.SuccessJSON(&SolanaPayPostResponse{Transaction: resp.TransactionBase64, Message: resp.Message})
}

func writeOrderSolanaPayError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, checkout.ErrCheckoutAttemptNotFound):
		r.ErrorCode(billing.CodeResourceNotFound, "order not found")
	case errors.Is(err, orders.ErrNotPayable), errors.Is(err, checkout.ErrCheckoutAttemptNotSolana):
		r.ErrorCode(billing.CodeOrderNotPayable, "the order takes no Solana payment now")
	case errors.Is(err, checkout.ErrCheckoutAttemptAlreadyCompleted), errors.Is(err, checkout.ErrCheckoutAttemptConflict), errors.Is(err, orders.ErrInProgress):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, checkout.ErrCheckoutAttemptValidation):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()))
	default:
		writeRefusal(r, err, "solana pay request failed")
	}
}
