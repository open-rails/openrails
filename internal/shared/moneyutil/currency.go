package moneyutil

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/currency"
)

// The currency registry sits in this leaf so every provider boundary can reach
// the one internal->rail converter without an import cycle. Currency is
// system-fixed, not merchant-scoped: the code is the authority (no DB CHECK).

// Currency is one registered currency and its native/settlement scale.
type Currency = currency.Units

// currencies mirrors the dependency-free currency registry, the one owner
// of every currency scale.
var currencies = func() map[string]Currency {
	out := map[string]Currency{}
	for _, units := range currency.List() {
		out[units.Code] = units
	}
	return out
}()

// NormalizeCurrency canonicalises a currency code to upper case. It lives in
// the leaf so write chokepoints can call it without importing money.
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

// FormatAmount renders native units exactly at the currency's registered scale,
// trimming trailing zeros down to a fiat minor unit: "19.99 USD",
// "0.000001 USD", "500 JPY", "1.5 SOL". An unregistered currency is named,
// never scaled.
func FormatAmount(amount int64, currency string) string {
	cur, ok := LookupCurrency(currency)
	if !ok {
		return fmt.Sprintf("%d units of unregistered currency %q", amount, currency)
	}
	whole, fraction, _ := strings.Cut(formatDecimal(amount, pow10(cur.Decimals), cur.Decimals), ".")
	shown := cur.MinorDecimals
	if cur.Kind == "crypto" {
		shown = 0
	}
	digits := min(max(len(strings.TrimRight(fraction, "0")), shown), cur.Decimals)
	if digits > 0 {
		whole += "." + fraction[:digits]
	}
	return whole + " " + cur.Code
}

// FormatAmounts renders per-currency totals in code order: "1.50 EUR, 12.00 USD".
func FormatAmounts(totals map[string]int64) string {
	codes := make([]string, 0, len(totals))
	for code := range totals {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	parts := make([]string, len(codes))
	for i, code := range codes {
		parts[i] = FormatAmount(totals[code], code)
	}
	return strings.Join(parts, ", ")
}

// FormatRailMinor renders a provider minor-unit amount (cents, whole yen,
// lamports) as FormatAmount does.
func FormatRailMinor(minor Cents, currency string) string {
	native, err := RailMinorToNative(currency, minor)
	if err != nil {
		return fmt.Sprintf("%d minor units of %q", minor, currency)
	}
	return FormatAmount(native, currency)
}

// FormatBaseUnits renders an on-chain token amount (lamports, USDC base units)
// as FormatAmount does.
func FormatBaseUnits(units uint64, currency string) string {
	if units > math.MaxInt64 {
		return fmt.Sprintf("%d base units of %q", units, currency)
	}
	return FormatRailMinor(Cents(units), currency) // #nosec G115 -- bounded above
}

// DescribeNativeScales lists native units per major unit for prose (LLM
// prompts): "EUR 1000000, JPY 10000, USD 1000000".
func DescribeNativeScales() string {
	parts := make([]string, 0, len(currencies))
	for _, code := range CurrencyCodes() {
		parts = append(parts, fmt.Sprintf("%s %d", code, pow10(currencies[code].Decimals)))
	}
	return strings.Join(parts, ", ")
}

// NativeToRailMinor converts an internal native amount to the rail minor unit
// (cents for USD, whole yen for JPY), rounding UP so a charge never
// under-covers it; amounts <= 0 give 0. Errors on an unregistered currency.
func NativeToRailMinor(currency string, amount int64) (Cents, error) {
	div, err := nativeDivisor(currency)
	if err != nil {
		return 0, err
	}
	if amount <= 0 {
		return 0, nil
	}
	if div < 0 {
		value, err := multiplyNative(amount, -div)
		return Cents(value), err
	}
	quotient := amount / div
	if amount%div != 0 {
		quotient++
	}
	return Cents(quotient), nil
}

// NativeToRailMinorExact is NativeToRailMinor without rounding, for prices: a
// sub-minor remainder or an unregistered currency is an error.
func NativeToRailMinorExact(currency string, amount int64) (Cents, error) {
	div, err := nativeDivisor(currency)
	if err != nil {
		return 0, err
	}
	if div < 0 {
		value, err := multiplyNative(amount, -div)
		return Cents(value), err
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
	return multiplyNative(int64(minor), div)
}

// DecimalToRailMinor is THE provider-decimal -> rail-minor boundary: it reads
// a provider's major-unit amount ("9.99", "500.00", "-5") as rail minor units
// of a registered currency, exactly. Zeros past the minor unit are accepted
// ("500.00" JPY is 500 yen); any other digit there, a blank or unknown
// currency, a malformed amount or int64 overflow is an error. An optional
// leading '-' is kept: callers that need a positive amount check it.
func DecimalToRailMinor(currency, amount string) (Cents, error) {
	cur, ok := LookupCurrency(currency)
	if !ok {
		return 0, fmt.Errorf("money: unknown currency %q", currency)
	}
	raw := strings.TrimSpace(amount)
	digits, sign := strings.CutPrefix(raw, "-")
	whole, fraction, _ := strings.Cut(digits, ".")
	if whole+fraction == "" || !allDigits(whole) || !allDigits(fraction) {
		return 0, fmt.Errorf("money: invalid decimal amount %q", amount)
	}
	if len(fraction) > cur.MinorDecimals {
		if strings.Trim(fraction[cur.MinorDecimals:], "0") != "" {
			return 0, fmt.Errorf("money: amount %q is not in %s %s", raw, cur.Code, minorUnitName(cur.Code))
		}
		fraction = fraction[:cur.MinorDecimals]
	}
	fraction += strings.Repeat("0", cur.MinorDecimals-len(fraction))
	value := whole + fraction
	if sign {
		value = "-" + value
	}
	minor, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money: amount %q exceeds int64", raw)
	}
	return Cents(minor), nil
}

func allDigits(s string) bool {
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// minorUnitName names a currency's rail minor unit for error messages: "whole
// cents" for 2-decimal currencies, "whole minor units" otherwise.
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
	pow := pow10(max(shift, -shift))
	if shift < 0 {
		return -pow, nil
	}
	return pow, nil
}

func pow10(n int) int64 {
	pow := int64(1)
	for range n {
		pow *= 10
	}
	return pow
}

func multiplyNative(amount, factor int64) (int64, error) {
	if factor <= 0 {
		return 0, fmt.Errorf("invalid currency scale factor")
	}
	if amount > math.MaxInt64/factor || amount < math.MinInt64/factor {
		return 0, fmt.Errorf("money amount exceeds int64 precision")
	}
	return amount * factor, nil
}
