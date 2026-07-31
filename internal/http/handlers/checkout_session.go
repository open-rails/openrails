package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

type checkoutSessionPaymentParams struct {
	Rail            string `json:"rail" binding:"required"`
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

	// SuccessURL / CancelURL are the post-checkout redirect targets for hosted
	// Stripe Checkout. The frontend supplies them (it knows its own origin);
	// #521 moved these off rail config and onto the request, but only the
	// direct CheckoutRequest carried them — the session path (this struct) had
	// no field, so the Stripe hosted flow always errored "success_url required".
	// Optional here because non-Stripe rails (Solana, NMI, CCBill) don't use them.
	SuccessURL string `json:"success_url,omitempty" binding:"omitempty,url"`
	CancelURL  string `json:"cancel_url,omitempty" binding:"omitempty,url"`
}

type checkoutSessionConfirmRequest struct {
	Payment struct {
		Rail      string `json:"rail" binding:"required,oneof=solana"`
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
	if err := checkoutRailUsable(r, req.Payment.Rail); err != nil {
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	req.IdempotencyKey = middleware.IdempotencyKeyFromRequest(r.Request)
	e2eRunID := strings.TrimSpace(r.Header("X-E2E-Run-ID"))
	if e2eRunID != "" {
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		if _, ok := req.Metadata["e2e_run_id"]; !ok {
			req.Metadata["e2e_run_id"] = e2eRunID
		}
	}
	svcReq := &checkout.CheckoutSessionCreateRequest{PriceID: req.PriceID, Mode: req.Mode, SubscriptionID: req.SubscriptionID, NewPriceID: req.NewPriceID, SuccessURL: req.SuccessURL, CancelURL: req.CancelURL, Metadata: req.Metadata, IdempotencyKey: req.IdempotencyKey, Payment: checkout.CheckoutSessionPaymentRequest{Rail: req.Payment.Rail, PaymentMethodID: req.Payment.PaymentMethodID, PaymentToken: req.Payment.PaymentToken, TokenSymbol: req.Payment.TokenSymbol, Flow: req.Payment.Flow, Wallet: req.Payment.Wallet, Email: req.Payment.Email, FirstName: req.Payment.FirstName, LastName: req.Payment.LastName, Address1: req.Payment.Address1, City: req.Payment.City, State: req.Payment.State, Zip: req.Payment.Zip, Country: req.Payment.Country, LastFour: req.Payment.LastFour, CardType: req.Payment.CardType, ExpiryDate: req.Payment.ExpiryDate}}
	resp, err := r.State.CheckoutSessionService.CreateSession(r.Request.Context(), svcReq, user)
	if err != nil {
		log.WithError(err).WithField("request_id", r.RequestID()).Error("Failed to create checkout session")
		// Card-abuse tracking (#371): a vault/card decline is a failed charge
		// attempt. Record it against this request's rate-limit subjects so
		// repeated failures escalate to captcha/block (and feed site-wide
		// attack-mode detection). Best-effort + nil-safe; never affects the
		// response.
		var vErr *paymentmethods.VaultError
		if errors.As(err, &vErr) {
			r.State.CardAbuseGuard.RecordChargeFailure(
				r.Request.Context(),
				middleware.SubjectKeysFromContext(r.Request.Context()),
			)
		}
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{
			Rail:   req.Payment.Rail,
			Wallet: req.Payment.Wallet,
		})
		return
	}
	r.SuccessJSON(resp)
}

// checkoutRailUsable pre-gates the wire payment.rail selector: a PSP key, or a
// rail kind resolving to exactly one armed PSP, must land on an armed account
// for the request merchant + environment (#848; #775/#788 — the ONE resolution
// seam via checkout's resolveRailTarget; how the account was armed, manifest
// or API, is invisible here). nil = usable; the error is the 400 message.
// Fail closed on missing wiring/merchant and on resolution errors.
func checkoutRailUsable(r *httprequest.Request, rail string) error {
	unsupported := errors.New("unsupported rail")
	if r == nil || r.State == nil || r.State.CheckoutService == nil {
		return unsupported
	}
	if _, ok := merchant.FromContext(r.Request.Context()); !ok {
		return unsupported
	}
	return r.State.CheckoutService.CheckoutRailUsable(r.Request.Context(), rail)
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
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{CheckoutSessionID: sessionID})
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
	svcReq := &checkout.CheckoutSessionConfirmRequest{Payment: checkout.CheckoutSessionConfirmPayment{Rail: req.Payment.Rail, Signature: req.Payment.Signature, Wallet: req.Payment.Wallet}}
	resp, err := r.State.CheckoutSessionService.ConfirmSession(r.Request.Context(), parsedID, svcReq, user)
	if err != nil {
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{
			Rail:              req.Payment.Rail,
			Wallet:            req.Payment.Wallet,
			CheckoutSessionID: sessionID,
		})
		return
	}
	r.SuccessJSON(resp)
}

// checkoutSessionErrorContext carries per-request context threaded into
// actionable checkout error metadata (e.g. the usdc_funding payload on the
// pre-flight insufficient-USDC 402), so the frontend can drive a funding flow.
type checkoutSessionErrorContext struct {
	Rail              string
	Wallet            string
	CheckoutSessionID string
}

func writeCheckoutSessionError(r *httprequest.Request, err error, ectx checkoutSessionErrorContext) {
	var vaultErr *paymentmethods.VaultError
	if errors.As(err, &vaultErr) {
		writeVaultError(r, vaultErr)
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
		need, have := insufficientUSDC.NeedBaseUnits, insufficientUSDC.HaveBaseUnits
		var short uint64
		if need > have {
			short = need - have
		}
		funding := map[string]any{
			"asset":                "USDC",
			"network":              "solana",
			"amount":               formatUSDCBaseUnits(need),
			"balance":              formatUSDCBaseUnits(have),
			"shortfall":            formatUSDCBaseUnits(short),
			"amount_base_units":    strconv.FormatUint(need, 10),
			"balance_base_units":   strconv.FormatUint(have, 10),
			"shortfall_base_units": strconv.FormatUint(short, 10),
		}
		if w := strings.TrimSpace(ectx.Wallet); w != "" {
			funding["wallet"] = w
		}
		if id := strings.TrimSpace(ectx.CheckoutSessionID); id != "" {
			funding["checkout_session_id"] = id
		}
		if p := strings.TrimSpace(ectx.Rail); p != "" {
			funding["rail"] = p
		}
		r.APIError(apiErr.WithMetadata(map[string]any{"usdc_funding": funding}))
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

// formatUSDCBaseUnits renders USDC token base units (6 decimals) as a trimmed
// decimal string: 1500000 -> "1.5", 250000 -> "0.25".
func formatUSDCBaseUnits(v uint64) string {
	whole := v / 1_000_000
	frac := v % 1_000_000
	if frac == 0 {
		return strconv.FormatUint(whole, 10)
	}
	return strconv.FormatUint(whole, 10) + "." + strings.TrimRight(fmt.Sprintf("%06d", frac), "0")
}
