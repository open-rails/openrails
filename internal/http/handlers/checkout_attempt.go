package handlers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/api"
	"github.com/open-rails/openrails/internal/billingauth"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
)

// checkoutVerifiedPrincipal is the customer route's verified customer as the
// payer the engine checks: interactive only for the customer's own sign-in.
func checkoutVerifiedPrincipal(r *httprequest.Request) billingauth.Payer {
	scope, ok := r.CustomerScope()
	if !ok {
		return billingauth.Payer{}
	}
	c, _ := billingauth.IdentityFromContext(r.Request.Context())
	return scope.Payer(c)
}

func writeCheckoutAttemptError(r *httprequest.Request, err error) {
	var blocked *checkout.CardAttemptsBlockedError
	if errors.As(err, &blocked) {
		writeCardAttemptsBlocked(r, blocked.RetryAfter)
		return
	}
	if errors.Is(err, billing.ErrIdempotencyKeyReused) {
		r.APIError(api.Coded(billing.CodeIdempotencyKeyReused, "idempotency key reused with different checkout attempt parameters"))
		return
	}
	var pmErr *paymentmethods.PaymentMethodError
	if errors.As(err, &pmErr) {
		writePaymentMethodError(r, pmErr)
		return
	}
	if writeCardEntryError(r, err) {
		return
	}
	if errors.Is(err, checkout.ErrPaymentMethodStale) {
		writePaymentMethodStale(r)
		return
	}
	if errors.Is(err, checkout.ErrPaymentMethodRequired) {
		writePaymentMethodRequired(r)
		return
	}
	// Insufficient USDC is an actionable customer state, not an internal
	// failure: 402 with the have/need amounts, so the client can offer to buy
	// USDC.
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
		r.APIError(apiErr.WithMetadata(map[string]any{"usdc_funding": funding}))
		return
	}
	switch {
	case errors.Is(err, vault.ErrUnavailable):
		// Before validation: a refused signer is unavailable, not a bad request.
		r.APIError(api.NewAPIError(http.StatusServiceUnavailable, api.ErrorTypeAPI, api.CodeServiceUnavailable, "payment signer is temporarily unavailable"))
	case errors.Is(err, checkout.ErrCheckoutCaptureUnavailable):
		r.APIError(api.NewAPIError(http.StatusServiceUnavailable, api.ErrorTypeAPI, "custodian_capture_unavailable", "custodian capture is unavailable"))
	case errors.Is(err, checkout.ErrCheckoutAttemptNotFound):
		r.ErrorCode(billing.CodeResourceNotFound, err.Error())
	case errors.Is(err, checkout.ErrCheckoutAttemptForbidden):
		r.ErrorCode(billing.CodeResourceAccessDenied, err.Error())
	case errors.Is(err, checkout.ErrCheckoutAttemptExpired):
		r.ErrorCode("checkout_attempt_expired", err.Error())
	case errors.Is(err, checkout.ErrCheckoutAttemptPending), errors.Is(err, checkout.ErrCheckoutProcessing):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, checkout.ErrCheckoutAttemptConflict):
		r.ErrorCode(billing.CodeResourceConflict, err.Error())
	case errors.Is(err, checkout.ErrCheckoutAttemptValidation):
		r.ErrorCode(billing.CodeInvalidParam, err.Error())
	default:
		writeRefusal(r, err, "checkout attempt request failed")
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
