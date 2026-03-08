package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/services"
	"github.com/open-rails/openrails/pkg/api"
)

// SolanaPayGetResponse is the response for GET /v1/checkout/:id/solana-pay
// per the Solana Pay Transaction Request specification.
type SolanaPayGetResponse struct {
	Label string `json:"label"`
	Icon  string `json:"icon"`
}

// SolanaPayPostRequest is the request body for POST /v1/checkout/:id/solana-pay
type SolanaPayPostRequest struct {
	Account string `json:"account" binding:"required"`
}

// SolanaPayPostResponse is the response for POST /v1/checkout/:id/solana-pay
// per the Solana Pay Transaction Request specification.
type SolanaPayPostResponse struct {
	Transaction string `json:"transaction"`
	Message     string `json:"message,omitempty"`
}

// GetSolanaPay handles GET /v1/checkout/:id/solana-pay
// Returns label and icon per Solana Pay Transaction Request spec.
// This endpoint is called by wallets when scanning a Solana Pay QR code.
func GetSolanaPay(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.GinCtx.Param("id"))
	if sessionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "id is required")
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

	// Validate the session exists and is a Solana session
	session, err := r.State.CheckoutSessionService.GetSessionForSolanaPay(r.Request.Context(), parsedID)
	if err != nil {
		writeSolanaPayError(r, err)
		return
	}

	// Return label and icon from store config
	label := "Payment"
	icon := ""
	if r.State.Config != nil && r.State.Config.Store != nil {
		if r.State.Config.Store.Name != "" {
			label = r.State.Config.Store.Name
		}
		icon = r.State.Config.Store.LogoURL
	}

	// Include product name in label if available (overrides store name)
	if session.ProductName != "" {
		label = session.ProductName
	}

	r.SuccessJSON(&SolanaPayGetResponse{
		Label: label,
		Icon:  icon,
	})
}

// PostSolanaPay handles POST /v1/checkout/:id/solana-pay
// Accepts wallet account, builds transaction, returns base64-encoded transaction.
// This endpoint is called by wallets to get a transaction to sign.
func PostSolanaPay(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.GinCtx.Param("id"))
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

	parsedID, err := api.ParseCheckoutSessionID(sessionID)
	if err != nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid checkout session id")
		return
	}

	// Build transaction for this account
	resp, err := r.State.CheckoutSessionService.BuildSolanaPayTransaction(r.Request.Context(), parsedID, req.Account)
	if err != nil {
		writeSolanaPayError(r, err)
		return
	}

	r.SuccessJSON(&SolanaPayPostResponse{
		Transaction: resp.TransactionBase64,
		Message:     resp.Message,
	})
}

func writeSolanaPayError(r *httprequest.Request, err error) {
	switch {
	case err == services.ErrCheckoutSessionNotFound:
		r.ErrorJSON(http.StatusNotFound, "checkout session not found")
	case err == services.ErrCheckoutSessionExpired:
		r.ErrorJSON(http.StatusGone, "checkout session expired")
	case err == services.ErrCheckoutSessionNotSolana:
		r.ErrorJSON(http.StatusBadRequest, "not a solana checkout session")
	case err == services.ErrCheckoutSessionAlreadyCompleted:
		r.ErrorJSON(http.StatusConflict, "checkout session already completed")
	default:
		r.ErrorJSON(http.StatusInternalServerError, "failed to process request")
	}
}
