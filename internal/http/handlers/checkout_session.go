package handlers

import (
	"errors"
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

type checkoutSessionPaymentParams struct {
	Processor       string `json:"processor" binding:"required,oneof=mobius ccbill solana stripe"`
	PaymentMethodID string `json:"payment_method_id,omitempty" binding:"omitempty"`
	PaymentToken    string `json:"payment_token,omitempty"`
	TokenSymbol     string `json:"token_symbol,omitempty" binding:"omitempty"`
	Flow            string `json:"flow,omitempty" binding:"omitempty,oneof=transfer_request transaction_request"`
	Wallet          string `json:"wallet,omitempty" binding:"omitempty"`
	Email           string `json:"email,omitempty" binding:"omitempty,email"`
	FirstName       string `json:"first_name,omitempty" binding:"omitempty,max=100"`
	LastName        string `json:"last_name,omitempty" binding:"omitempty,max=100"`
	Address1        string `json:"address1,omitempty" binding:"omitempty,max=200"`
	City            string `json:"city,omitempty" binding:"omitempty,max=100"`
	State           string `json:"state,omitempty" binding:"omitempty,max=50"`
	Zip             string `json:"zip,omitempty" binding:"omitempty,max=20"`
	Country         string `json:"country,omitempty" binding:"omitempty,max=2"`
	LastFour        string `json:"last_four,omitempty" binding:"omitempty"`
	CardType        string `json:"card_type,omitempty" binding:"omitempty"`
	ExpiryDate      string `json:"expiry_date,omitempty" binding:"omitempty"`
}

type checkoutSessionCreateRequest struct {
	PriceID        string                       `json:"price_id" binding:"required"`
	Mode           string                       `json:"mode,omitempty" binding:"omitempty,oneof=one_off subscription"`
	Payment        checkoutSessionPaymentParams `json:"payment" binding:"required"`
	Metadata       map[string]string            `json:"metadata,omitempty"`
	IdempotencyKey string                       `json:"-"`
}

type checkoutSessionConfirmRequest struct {
	Payment struct {
		Processor string `json:"processor" binding:"required,oneof=solana"`
		Signature string `json:"signature,omitempty"`
		Wallet    string `json:"wallet,omitempty"`
	} `json:"payment" binding:"required"`
}

func CreateCheckoutSession(r *httprequest.Request) {
	var req checkoutSessionCreateRequest
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
	svcReq := &payments.CheckoutSessionCreateRequest{PriceID: req.PriceID, Mode: req.Mode, Metadata: req.Metadata, IdempotencyKey: req.IdempotencyKey, Payment: payments.CheckoutSessionPaymentRequest{Processor: req.Payment.Processor, PaymentMethodID: req.Payment.PaymentMethodID, PaymentToken: req.Payment.PaymentToken, TokenSymbol: req.Payment.TokenSymbol, Flow: req.Payment.Flow, Wallet: req.Payment.Wallet, Email: req.Payment.Email, FirstName: req.Payment.FirstName, LastName: req.Payment.LastName, Address1: req.Payment.Address1, City: req.Payment.City, State: req.Payment.State, Zip: req.Payment.Zip, Country: req.Payment.Country, LastFour: req.Payment.LastFour, CardType: req.Payment.CardType, ExpiryDate: req.Payment.ExpiryDate}}
	resp, err := r.State.CheckoutSessionService.CreateSession(r.Request.Context(), svcReq, user)
	if err != nil {
		log.WithError(err).Error("Failed to create checkout session")
		writeCheckoutSessionError(r, err)
		return
	}
	r.SuccessJSON(resp)
}

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

func ConfirmCheckoutSession(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.GinCtx.Param("id"))
	if sessionID == "" {
		r.ErrorJSON(http.StatusBadRequest, "id is required")
		return
	}
	var req checkoutSessionConfirmRequest
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
	svcReq := &payments.CheckoutSessionConfirmRequest{Payment: payments.CheckoutSessionConfirmPayment{Processor: req.Payment.Processor, Signature: req.Payment.Signature, Wallet: req.Payment.Wallet}}
	resp, err := r.State.CheckoutSessionService.ConfirmSession(r.Request.Context(), parsedID, svcReq, user)
	if err != nil {
		writeCheckoutSessionError(r, err)
		return
	}
	r.SuccessJSON(resp)
}

func writeCheckoutSessionError(r *httprequest.Request, err error) {
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
	case errors.Is(err, payments.ErrCheckoutSessionNotFound):
		r.ErrorJSON(http.StatusNotFound, err.Error())
	case errors.Is(err, payments.ErrCheckoutSessionForbidden):
		r.ErrorJSON(http.StatusForbidden, err.Error())
	case errors.Is(err, payments.ErrCheckoutSessionExpired):
		r.ErrorJSON(http.StatusGone, err.Error())
	case errors.Is(err, payments.ErrCheckoutSessionPending):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, payments.ErrCheckoutSessionConflict):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, payments.ErrCheckoutSessionValidation):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		r.ErrorJSON(http.StatusInternalServerError, "checkout session request failed")
	}
}
