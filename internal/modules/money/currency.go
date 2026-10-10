package money

import (
	"fmt"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// The currency registry lives in internal/shared/moneyutil; this file is the
// billing policy on top of it.

// DefaultCurrency is the explicit USD currency code used by callers that want
// USD. It is a declared config default (the FX base/accounting unit), never a
// substitute for a currency a payment, price or transaction failed to carry.
const DefaultCurrency = "USD"

// normalizeCurrency upper-cases the code, so built-in currency codes are
// case-insensitive ("usd" == "USD"). Registry keys are upper.
func normalizeCurrency(c string) string {
	return moneyutil.NormalizeCurrency(c)
}

// NormalizeCurrency upper-cases built-in currency codes. It is exported for
// sibling modules that key native-money rows by currency but still rely on this
// package's registry as the source of truth.
func NormalizeCurrency(c string) string {
	return normalizeCurrency(c)
}

// RequireBillingCurrency validates a currency against the supported registry.
func RequireBillingCurrency(code string) error {
	return moneyutil.ValidateCurrency(code)
}

// CurrencyDecimals returns the scale of a supported currency.
func CurrencyDecimals(code string) (int, error) {
	decimals, ok := moneyutil.CurrencyScale(code)
	if !ok {
		return 0, fmt.Errorf("money: unknown currency %q", code)
	}
	return decimals, nil
}
