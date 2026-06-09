package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

type checkoutSessionPaymentParams struct {
	Processor       string `json:"processor" binding:"required"`
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
	// PriceID is required for purchase/subscribe modes. For solana_cancel /
	// solana_tier_change it is optional (cancel uses the subscription's current
	// price; tier-change uses new_price_id).
	PriceID        string                       `json:"price_id,omitempty" binding:"omitempty"`
	Mode           string                       `json:"mode,omitempty" binding:"omitempty,oneof=one_off subscription solana_cancel solana_tier_change"`
	Payment        checkoutSessionPaymentParams `json:"payment" binding:"required"`
	Metadata       map[string]string            `json:"metadata,omitempty"`
	IdempotencyKey string                       `json:"-"`

	// SubscriptionID is required for the solana_cancel / solana_tier_change modes:
	// the caller's existing Solana subscription to act on (ownership enforced).
	SubscriptionID string `json:"subscription_id,omitempty" binding:"omitempty"`
	// NewPriceID is required for the solana_tier_change mode: the price to change to.
	NewPriceID string `json:"new_price_id,omitempty" binding:"omitempty"`
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
	if !processors.IsConfigured(r.State.Config, req.Payment.Processor) {
		r.ErrorJSON(http.StatusBadRequest, "unsupported processor")
		return
	}
	req.IdempotencyKey = r.Header("X-Idempotency-Key")
	e2eRunID := strings.TrimSpace(r.Header("X-E2E-Run-ID"))
	if e2eRunID != "" {
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		if _, ok := req.Metadata["e2e_run_id"]; !ok {
			req.Metadata["e2e_run_id"] = e2eRunID
		}
	}
	svcReq := &checkout.CheckoutSessionCreateRequest{PriceID: req.PriceID, Mode: req.Mode, SubscriptionID: req.SubscriptionID, NewPriceID: req.NewPriceID, Metadata: req.Metadata, IdempotencyKey: req.IdempotencyKey, Payment: checkout.CheckoutSessionPaymentRequest{Processor: req.Payment.Processor, PaymentMethodID: req.Payment.PaymentMethodID, PaymentToken: req.Payment.PaymentToken, TokenSymbol: req.Payment.TokenSymbol, Flow: req.Payment.Flow, Wallet: req.Payment.Wallet, Email: req.Payment.Email, FirstName: req.Payment.FirstName, LastName: req.Payment.LastName, Address1: req.Payment.Address1, City: req.Payment.City, State: req.Payment.State, Zip: req.Payment.Zip, Country: req.Payment.Country, LastFour: req.Payment.LastFour, CardType: req.Payment.CardType, ExpiryDate: req.Payment.ExpiryDate}}
	resp, err := r.State.CheckoutSessionService.CreateSession(r.Request.Context(), svcReq, user)
	if err != nil {
		log.WithError(err).Error("Failed to create checkout session")
		// Card-abuse tracking (#371): a vault/card decline is a failed charge
		// attempt. Record it against this request's rate-limit subjects so
		// repeated failures escalate to captcha/block (and feed attack-mode
		// detection). Best-effort + nil-safe; never affects the response.
		var vErr *vault.VaultError
		if errors.As(err, &vErr) {
			r.State.CardAbuseGuard.RecordChargeFailure(
				r.Request.Context(),
				ginmw.SubjectKeysFromContext(r.Request.Context()),
			)
		}
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{
			Processor: req.Payment.Processor,
			Wallet:    req.Payment.Wallet,
			Asset:     req.Payment.TokenSymbol,
		})
		return
	}
	r.SuccessJSON(resp)
}
func GetCheckoutSession(r *httprequest.Request) {
	sessionID := strings.TrimSpace(r.Param("id"))
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
	sessionID := strings.TrimSpace(r.Param("id"))
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
	svcReq := &checkout.CheckoutSessionConfirmRequest{Payment: checkout.CheckoutSessionConfirmPayment{Processor: req.Payment.Processor, Signature: req.Payment.Signature, Wallet: req.Payment.Wallet}}
	resp, err := r.State.CheckoutSessionService.ConfirmSession(r.Request.Context(), parsedID, svcReq, user)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{
			Processor:         req.Payment.Processor,
			Wallet:            req.Payment.Wallet,
			CheckoutSessionID: sessionID,
		})
		return
	}
	r.SuccessJSON(resp)
}

type checkoutSessionErrorContext struct {
	Processor         string
	Wallet            string
	Asset             string
	CheckoutSessionID string
}

func writeCheckoutSessionError(r *httprequest.Request, err error, contexts ...checkoutSessionErrorContext) {
	var vaultErr *vault.VaultError
	if errors.As(err, &vaultErr) {
		code := api.CodePaymentFailed
		if strings.TrimSpace(vaultErr.LocalizationID) != "" {
			code = vaultErr.LocalizationID
		}
		r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, code, vaultErr.Error()))
		return
	}
	// Pre-flight insufficient-USDC (#286): a typed, actionable user state (NOT an
	// internal failure). Surface a clear payment-error code + the have/need amounts
	// so the frontend can show "need $X, have $Y -> buy USDC" (MoonPay).
	var insufficientUSDC *recurring.InsufficientUSDCError
	if errors.As(err, &insufficientUSDC) {
		param := "usdc_balance"
		apiErr := api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, api.CodeInsufficientFunds, insufficientUSDC.Error())
		apiErr.Param = &param
		apiErr.Metadata = insufficientUSDCFundingMetadata(insufficientUSDC, contexts...)
		r.APIError(apiErr)
		return
	}
	switch {
	case errors.Is(err, checkout.ErrCheckoutSessionNotFound):
		r.ErrorJSON(http.StatusNotFound, err.Error())
	case errors.Is(err, checkout.ErrCheckoutSessionForbidden):
		r.ErrorJSON(http.StatusForbidden, err.Error())
	case errors.Is(err, checkout.ErrCheckoutSessionExpired):
		r.ErrorJSON(http.StatusGone, err.Error())
	case errors.Is(err, checkout.ErrCheckoutSessionPending):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, checkout.ErrCheckoutSessionConflict):
		r.ErrorJSON(http.StatusConflict, err.Error())
	case errors.Is(err, checkout.ErrCheckoutSessionValidation):
		r.ErrorJSON(http.StatusBadRequest, err.Error())
	default:
		r.ErrorJSON(http.StatusInternalServerError, "checkout session request failed")
	}
}

func insufficientUSDCFundingMetadata(err *recurring.InsufficientUSDCError, contexts ...checkoutSessionErrorContext) map[string]any {
	ctx := checkoutSessionErrorContext{}
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	asset := strings.ToUpper(strings.TrimSpace(ctx.Asset))
	if asset == "" {
		asset = "USDC"
	}
	network := ""
	if strings.EqualFold(strings.TrimSpace(ctx.Processor), "solana") {
		network = "solana"
	}
	if network == "" {
		network = "solana"
	}
	shortfall := uint64(0)
	if err.NeedBaseUnits > err.HaveBaseUnits {
		shortfall = err.NeedBaseUnits - err.HaveBaseUnits
	}
	funding := map[string]any{
		"asset":                asset,
		"network":              network,
		"amount":               formatUSDCBaseUnits(err.NeedBaseUnits),
		"amount_needed":        formatUSDCBaseUnits(err.NeedBaseUnits),
		"balance":              formatUSDCBaseUnits(err.HaveBaseUnits),
		"shortfall":            formatUSDCBaseUnits(shortfall),
		"amount_base_units":    strconv.FormatUint(err.NeedBaseUnits, 10),
		"balance_base_units":   strconv.FormatUint(err.HaveBaseUnits, 10),
		"shortfall_base_units": strconv.FormatUint(shortfall, 10),
	}
	if wallet := strings.TrimSpace(ctx.Wallet); wallet != "" {
		funding["wallet"] = wallet
	}
	if checkoutSessionID := strings.TrimSpace(ctx.CheckoutSessionID); checkoutSessionID != "" {
		funding["checkout_session_id"] = checkoutSessionID
	}
	return map[string]any{"usdc_funding": funding}
}

func formatUSDCBaseUnits(baseUnits uint64) string {
	const decimals = 6
	whole := baseUnits / 1_000_000
	frac := baseUnits % 1_000_000
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	value := strconv.FormatUint(whole, 10) + "." + leftPadUint(frac, decimals)
	return strings.TrimRight(value, "0")
}

func leftPadUint(value uint64, width int) string {
	out := strconv.FormatUint(value, 10)
	for len(out) < width {
		out = "0" + out
	}
	return out
}
