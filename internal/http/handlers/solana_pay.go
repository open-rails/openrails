package handlers

import (
	"errors"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/invoices"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
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

// solanaInvoiceID is the invoice a wallet's request names, "" after answering.
func solanaInvoiceID(r *httprequest.Request) (billing.InvoiceID, bool) {
	id, err := billing.ParseInvoiceID(strings.TrimSpace(r.Param("id")))
	if err != nil || id.IsZero() {
		r.ErrorCode(billing.CodeResourceNotFound, "invoice not found")
		return billing.InvoiceID{}, false
	}
	if r.State.InvoiceAttemptService == nil {
		r.ErrorCode(billing.CodeServiceUnavailable, "solana pay unavailable")
		return billing.InvoiceID{}, false
	}
	return id, true
}

// GetInvoiceSolanaPay handles GET /v1/invoices/{id}/solana-pay: what a wallet
// shows before it asks for the invoice's transaction.
func GetInvoiceSolanaPay(r *httprequest.Request) {
	id, ok := solanaInvoiceID(r)
	if !ok {
		return
	}
	items, err := r.State.InvoiceAttemptService.InvoiceSolanaLabel(r.Request.Context(), id.UUID())
	if err != nil {
		writeInvoiceSolanaPayError(r, err)
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

// PostInvoiceSolanaPay handles POST /v1/invoices/{id}/solana-pay: the wallet's
// account in, the invoice's transaction out.
func PostInvoiceSolanaPay(r *httprequest.Request) {
	var req SolanaPayPostRequest
	if !r.BindJSON(&req) {
		return
	}
	id, ok := solanaInvoiceID(r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Account) == "" {
		r.APIError(api.Coded(billing.CodeInvalidParam, "account is required").WithParam("account"))
		return
	}
	resp, err := r.State.InvoiceAttemptService.BuildInvoiceSolanaTransaction(r.Request.Context(), id.UUID(), req.Account)
	if err != nil {
		writeInvoiceSolanaPayError(r, err)
		return
	}
	r.SuccessJSON(&SolanaPayPostResponse{Transaction: resp.TransactionBase64, Message: resp.Message})
}

func writeInvoiceSolanaPayError(r *httprequest.Request, err error) {
	switch {
	case errors.Is(err, payment.ErrInvoiceAttemptNotFound):
		r.ErrorCode(billing.CodeResourceNotFound, "invoice not found")
	case errors.Is(err, invoices.ErrNotPayable), errors.Is(err, payment.ErrInvoiceAttemptNotSolana):
		r.ErrorCode(billing.CodeInvoiceNotPayable, "the invoice takes no Solana payment now")
	case errors.Is(err, payment.ErrInvoiceAttemptAlreadyCompleted), errors.Is(err, payment.ErrInvoiceAttemptConflict), errors.Is(err, invoices.ErrInProgress):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, payment.ErrInvoiceAttemptValidation):
		r.APIError(api.Coded(billing.CodeInvalidParam, err.Error()))
	default:
		writeRefusal(r, err, "solana pay request failed")
	}
}
