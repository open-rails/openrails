package money

import (
	"fmt"
	"sort"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// System currency registry (#472). Currency is system-fixed, NOT merchant-scoped:
// the codebase is the authority (there is no DB CHECK). amount columns are
// integers in the currency's registered internal precision.

// DefaultCurrency is the explicit USD currency code used by callers that want USD.
const DefaultCurrency = "USD"

// Currency is a system currency code and its minor-unit scale.
type Currency struct {
	Code          string
	Decimals      int    // internal units per major unit = 10^Decimals
	MinorDecimals int    // ISO-4217/provider minor units per major unit = 10^MinorDecimals (0 for zero-decimal rails like JPY)
	Kind          string // "fiat"
}

var currencies = map[string]Currency{
	"USD": {Code: "USD", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
	"EUR": {Code: "EUR", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
	"JPY": {Code: "JPY", Decimals: 4, MinorDecimals: 0, Kind: "fiat"},
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

// NativeToRailMinor is THE internal->provider amount converter (#671): it
// converts an internal native amount (10^Decimals units per major unit) into
// the provider/rail minor unit (10^MinorDecimals per major unit — cents for
// USD/EUR, whole yen for zero-decimal JPY; typed Cents). Rounds UP so the
// charge always covers the internal amount (never under-charges); the
// sub-minor remainder (< one rail minor unit) is the customer's gain. Errors
// on an unregistered currency — callers must not guess a scale.
func NativeToRailMinor(currency string, amount int64) (moneyutil.Cents, error) {
	cur, ok := currencies[normalizeCurrency(currency)]
	if !ok {
		return 0, fmt.Errorf("money: unknown currency %q", currency)
	}
	if amount <= 0 {
		return 0, nil
	}
	shift := cur.Decimals - cur.MinorDecimals
	if shift <= 0 {
		out := amount
		for range -shift {
			out *= 10
		}
		return moneyutil.Cents(out), nil
	}
	div := int64(1)
	for range shift {
		div *= 10
	}
	return moneyutil.Cents((amount + div - 1) / div), nil
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
