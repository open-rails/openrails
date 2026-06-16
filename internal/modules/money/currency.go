package money

import (
	"fmt"
	"sort"
	"strings"
)

// System currency registry (#472). Currency is system-fixed, NOT merchant-scoped:
// the codebase is the authority (there is no DB CHECK). amount columns are
// integers in the currency's registered internal precision.

// DefaultCurrency is the explicit USD currency code used by callers that want USD.
const DefaultCurrency = "USD"

// Currency is a system currency code and its minor-unit scale.
type Currency struct {
	Code     string
	Decimals int    // internal units per major unit = 10^Decimals
	Kind     string // "fiat" | "crypto"
}

var currencies = map[string]Currency{
	"USD":  {Code: "USD", Decimals: 6, Kind: "fiat"},
	"USDC": {Code: "USDC", Decimals: 6, Kind: "crypto"},
	"EUR":  {Code: "EUR", Decimals: 6, Kind: "fiat"},
	"JPY":  {Code: "JPY", Decimals: 4, Kind: "fiat"},
	"SOL":  {Code: "SOL", Decimals: 9, Kind: "crypto"},
}

// normalizeCurrency upper-cases the code, so built-in currency codes are
// case-insensitive ("usd" == "USD"). Registry keys are upper.
func normalizeCurrency(c string) string {
	return strings.ToUpper(strings.TrimSpace(c))
}

// NormalizeCurrency upper-cases built-in currency codes. It is exported for
// sibling modules that key native-money rows by currency but still rely on this
// package's registry as the source of truth.
func NormalizeCurrency(c string) string {
	return normalizeCurrency(c)
}

// ValidateCurrency errors on a blank or unknown code.
func ValidateCurrency(c string) error {
	if _, ok := currencies[normalizeCurrency(c)]; !ok {
		return fmt.Errorf("money: unknown currency %q", c)
	}
	return nil
}

// CurrencyScale returns the currency's internal precision decimals.
func CurrencyScale(c string) (int, bool) {
	cur, ok := currencies[normalizeCurrency(c)]
	return cur.Decimals, ok
}

// CurrencyCodes returns the registered native currencies in deterministic order.
func CurrencyCodes() []string {
	out := make([]string, 0, len(currencies))
	for code := range currencies {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

// RequireBillingCurrency enforces the #474/#475 invariant at the billing layer:
// billing (invoice/owed/charge/auto-topup/account-settings) is external-currency-
// only, so a qualified custom-credit code (merchant/name, #475) is REJECTED here.
// Ledger primitives (Deposit/Withdraw/Hold) keep using the looser ValidateCurrency.
func RequireBillingCurrency(code string) error {
	if IsQualifiedUnit(code) {
		return fmt.Errorf("%w: %q", ErrBillingUnitRequired, code)
	}
	return ValidateCurrency(code)
}
