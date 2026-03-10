package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/api"
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
	session, err := r.State.CheckoutSessionService.GetSessionForSolanaPay(r.Request.Context(), parsedID)
	if err != nil {
		writeSolanaPayError(r, err)
		return
	}
	label := "Payment"
	icon := ""
	if r.State.Config != nil && r.State.Config.Store != nil {
		if r.State.Config.Store.Name != "" {
			label = r.State.Config.Store.Name
		}
		icon = r.State.Config.Store.LogoURL
	}
	if session.ProductName != "" {
		label = session.ProductName
	}
	r.SuccessJSON(&SolanaPayGetResponse{Label: label, Icon: icon})
}

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
	resp, err := r.State.CheckoutSessionService.BuildSolanaPayTransaction(r.Request.Context(), parsedID, req.Account)
	if err != nil {
		writeSolanaPayError(r, err)
		return
	}
	r.SuccessJSON(&SolanaPayPostResponse{Transaction: resp.TransactionBase64, Message: resp.Message})
}

func writeSolanaPayError(r *httprequest.Request, err error) {
	switch {
	case err == payments.ErrCheckoutSessionNotFound:
		r.ErrorJSON(http.StatusNotFound, "checkout session not found")
	case err == payments.ErrCheckoutSessionExpired:
		r.ErrorJSON(http.StatusGone, "checkout session expired")
	case err == payments.ErrCheckoutSessionNotSolana:
		r.ErrorJSON(http.StatusBadRequest, "not a solana checkout session")
	case err == payments.ErrCheckoutSessionAlreadyCompleted:
		r.ErrorJSON(http.StatusConflict, "checkout session already completed")
	default:
		r.ErrorJSON(http.StatusInternalServerError, "failed to process request")
	}
}
