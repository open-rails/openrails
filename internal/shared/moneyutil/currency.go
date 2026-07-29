package moneyutil

import (
	"fmt"
	"sort"
	"strings"
)

// System currency registry (#472), moved here from internal/modules/money by
// or#863. It is a zero-dependency scale table, and it belongs in the leaf money
// package for one concrete reason: the single internal->rail converter has to
// be reachable from EVERY provider boundary. It was not. internal/modules/money
// imports internal/modules/subscriptions, so subscriptions can never import
// money back — which is exactly why the two NMI plan-migration pushes hardcoded
// a divide-by-10 000 instead of asking the registry. Currency is system-fixed,
// NOT merchant-scoped: the codebase is the authority (there is no DB CHECK).

// Currency is a system currency code and its minor-unit scale.
type Currency struct {
	Code          string
	Decimals      int    // internal units per major unit = 10^Decimals
	MinorDecimals int    // ISO-4217/provider minor units per major unit = 10^MinorDecimals (0 for zero-decimal rails like JPY)
	Kind          string // "fiat"
}

// NativeShift is the decimal shift between a currency's internal scale and its
// rail minor unit: internal = minor * 10^NativeShift.
func (c Currency) NativeShift() int { return c.Decimals - c.MinorDecimals }

var currencies = map[string]Currency{
	"USD": {Code: "USD", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
	"EUR": {Code: "EUR", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
	"JPY": {Code: "JPY", Decimals: 4, MinorDecimals: 0, Kind: "fiat"},
}

// NormalizeCurrency canonicalises a currency code to UPPER case (CUR-6).
// It lives here, in the leaf, rather than in the registry package, because it
// is a pure string operation with no registry dependency — and because the
// repo-level write chokepoints that must call it (payments, prices) cannot
// import internal/modules/money without an import cycle. ONE definition:
// money.NormalizeCurrency delegates here.
func NormalizeCurrency(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// LookupCurrency returns the registered currency for a code.
func LookupCurrency(code string) (Currency, bool) {
	cur, ok := currencies[NormalizeCurrency(code)]
	return cur, ok
}

// ValidateCurrency errors on a blank or unknown code.
func ValidateCurrency(code string) error {
	if _, ok := currencies[NormalizeCurrency(code)]; !ok {
		return fmt.Errorf("money: unknown currency %q", code)
	}
	return nil
}

// CurrencyScale returns the currency's internal precision decimals.
func CurrencyScale(code string) (int, bool) {
	cur, ok := currencies[NormalizeCurrency(code)]
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

// NativeToRailMinor is THE internal->provider amount converter (#671): it
// converts an internal native amount (10^Decimals units per major unit) into
// the provider/rail minor unit (10^MinorDecimals per major unit — cents for
// USD/EUR, whole yen for zero-decimal JPY; typed Cents). Rounds UP so the
// charge always covers the internal amount (never under-charges); the
// sub-minor remainder (< one rail minor unit) is the customer's gain. Errors
// on an unregistered currency — callers must not guess a scale.
func NativeToRailMinor(currency string, amount int64) (Cents, error) {
	div, err := nativeDivisor(currency)
	if err != nil {
		return 0, err
	}
	if amount <= 0 {
		return 0, nil
	}
	if div < 0 {
		return Cents(amount * -div), nil
	}
	return Cents((amount + div - 1) / div), nil
}

// NativeToRailMinorExact is NativeToRailMinor's no-rounding sibling: the
// conversion used where an amount is a PRICE rather than a computed accrual, so
// a sub-minor remainder is a defect to surface, never a rounding to absorb.
// Same registry, same refusal on an unregistered currency — which is the whole
// point: a hardcoded /10_000 cannot refuse, and so happily converts an amount
// whose currency nobody established.
func NativeToRailMinorExact(currency string, amount int64) (Cents, error) {
	div, err := nativeDivisor(currency)
	if err != nil {
		return 0, err
	}
	if div < 0 {
		return Cents(amount * -div), nil
	}
	if amount%div != 0 {
		return 0, fmt.Errorf("amount %d internal units is not representable in %s %s",
			amount, NormalizeCurrency(currency), minorUnitName(currency))
	}
	return Cents(amount / div), nil
}

// RailMinorToNative widens a provider/rail minor amount back into the
// currency's internal native scale. The inbound twin of NativeToRailMinor.
func RailMinorToNative(currency string, minor Cents) (int64, error) {
	div, err := nativeDivisor(currency)
	if err != nil {
		return 0, err
	}
	if div < 0 {
		return int64(minor) / -div, nil
	}
	return int64(minor) * div, nil
}

// minorUnitName names a currency's rail minor unit for error messages: "whole
// cents" for the 2-decimal majority, the generic term otherwise (whole yen is
// not a cent, and saying so would be the same kind of small lie this issue is
// about).
func minorUnitName(currency string) string {
	if cur, ok := LookupCurrency(currency); ok && cur.MinorDecimals == 2 {
		return "whole cents"
	}
	return "whole minor units"
}

// nativeDivisor returns 10^shift for a registered currency, or the NEGATIVE
// multiplier when the internal scale is coarser than the rail's (shift < 0).
func nativeDivisor(currency string) (int64, error) {
	cur, ok := currencies[NormalizeCurrency(currency)]
	if !ok {
		return 0, fmt.Errorf("money: unknown currency %q", currency)
	}
	shift := cur.NativeShift()
	pow := int64(1)
	for range max(shift, -shift) {
		pow *= 10
	}
	if shift < 0 {
		return -pow, nil
	}
	return pow, nil
}
