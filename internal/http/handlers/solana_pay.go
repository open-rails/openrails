package handlers

import (
	"errors"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/orders"
	"github.com/open-rails/openrails/internal/modules/payment"
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

// solanaOrderID is the order a wallet's request names, "" after answering.
func solanaOrderID(r *httprequest.Request) (billing.OrderID, bool) {
	id, err := billing.ParseOrderID(strings.TrimSpace(r.Param("id")))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeResourceNotFound, "order not found")
		return billing.OrderID{}, false
	}
	if r.State.OrderAttemptService == nil {
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
	items, err := r.State.OrderAttemptService.OrderSolanaLabel(r.Request.Context(), id.UUID())
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
	resp, err := r.State.OrderAttemptService.BuildOrderSolanaTransaction(r.Request.Context(), id.UUID(), req.Account)
	if err != nil {
		writeOrderSolanaPayError(r, err)
		return
	}
	r.SuccessJSON(&SolanaPayPostResponse{Transaction: resp.TransactionBase64, Message: resp.Message})
}

func writeOrderSolanaPayError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, payment.ErrOrderAttemptNotFound):
		r.ErrorCode(billing.CodeResourceNotFound, "order not found")
	case errors.Is(err, orders.ErrNotPayable), errors.Is(err, payment.ErrOrderAttemptNotSolana):
		r.ErrorCode(billing.CodeOrderNotPayable, "the order takes no Solana payment now")
	case errors.Is(err, payment.ErrOrderAttemptAlreadyCompleted), errors.Is(err, payment.ErrOrderAttemptConflict), errors.Is(err, orders.ErrInProgress):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, payment.ErrOrderAttemptValidation):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()))
	default:
		writeRefusal(r, err, "solana pay request failed")
	}
}
