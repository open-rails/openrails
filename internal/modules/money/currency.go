package money

import (
	"fmt"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// The system currency registry itself lives in internal/shared/moneyutil
// (or#863): a zero-dependency scale table has to sit BELOW every provider
// boundary that needs it, and this package cannot be one of those homes —
// internal/modules/money imports internal/modules/subscriptions, so the two
// NMI plan-migration pushes could never have called the converter here.
// What remains in this file is the BILLING doctrine layered on the registry.

// DefaultCurrency is the explicit USD currency code used by callers that want
// USD. It is a declared CONFIG default — the FX base/accounting unit — and
// never a substitute for a currency a payment, price or transaction failed to
// carry (CUR-9).
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

// RequireBillingCurrency enforces the #474/#475 invariant at the billing layer:
// billing (invoice/owed/charge/auto-topup/account-settings) is external-currency-
// only, so a qualified custom-credit code (merchant/name, #475) is REJECTED here.
// Ledger primitives (Deposit/Withdraw/Hold) keep using the looser ValidateCurrency.
func RequireBillingCurrency(code string) error {
	if IsQualifiedUnit(code) {
		return fmt.Errorf("%w: %q", ErrBillingUnitRequired, code)
	}
	return moneyutil.ValidateCurrency(code)
}
