package ccbill

import (
	"fmt"
	"sort"
	"strings"
)

// CCBill's FlexForm `currencyCode` parameter takes an ISO-4217 NUMERIC code,
// and CCBill bills exactly this set. Kept as STRINGS, not ints: AUD is "036"
// and the leading zero is part of the wire value.
//
// This is the single table for both directions — outbound (checkout picks the
// code from the price's currency) and inbound (the webhook maps the code CCBill
// reports back to the price's currency). One table means a currency we can bill
// is always a currency the webhook can match, so no charge can land and then be
// rejected as a mismatch (#819).
var flexFormCurrencyCodes = map[string]string{
	"aud": "036",
	"cad": "124",
	"eur": "978",
	"gbp": "826",
	"jpy": "392",
	"usd": "840",
}

// UnsupportedCurrencyError reports a price CCBill cannot bill. It is returned
// BEFORE any FlexForm URL exists — the customer can only be charged by loading
// that form, so refusing here refuses before the charge.
type UnsupportedCurrencyError struct {
	Currency string
}

func (e *UnsupportedCurrencyError) Error() string {
	if strings.TrimSpace(e.Currency) == "" {
		return fmt.Sprintf("ccbill: price has no currency; CCBill bills %s and a missing currency is never defaulted", strings.Join(SupportedCurrencies(), ", "))
	}
	return fmt.Sprintf("ccbill: currency %q cannot be billed on CCBill (supported: %s)", e.Currency, strings.Join(SupportedCurrencies(), ", "))
}

// CurrencyCode maps an ISO-4217 alpha-3 currency to the numeric code CCBill's
// FlexForm expects. There is no default: an empty or unbillable currency is an
// error, never a silent USD.
func CurrencyCode(currency string) (string, error) {
	code, ok := flexFormCurrencyCodes[strings.ToLower(strings.TrimSpace(currency))]
	if !ok {
		return "", &UnsupportedCurrencyError{Currency: currency}
	}
	return code, nil
}

// CurrencyFromCode is the inverse: the numeric code CCBill reports on a webhook
// back to the ISO alpha-3 currency prices are denominated in.
func CurrencyFromCode(code string) (string, bool) {
	code = strings.TrimSpace(code)
	for currency, numeric := range flexFormCurrencyCodes {
		if numeric == code {
			return currency, true
		}
	}
	return "", false
}

// SupportedCurrencies lists the billable currencies, sorted, for error copy and
// for callers that gate a catalog on rail capability.
func SupportedCurrencies() []string {
	out := make([]string, 0, len(flexFormCurrencyCodes))
	for currency := range flexFormCurrencyCodes {
		out = append(out, currency)
	}
	sort.Strings(out)
	return out
}
