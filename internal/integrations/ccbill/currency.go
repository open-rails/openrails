package ccbill

import (
	"fmt"
	"sort"
	"strings"
)

// flexFormCurrencyCodes is CCBill's billable set: canonical (upper-case, as the
// DB currency CHECK accepts) currency -> ISO-4217 numeric `currencyCode`.
// Values are strings: AUD's leading zero ("036") is part of the wire value.
// Checkout and the webhook share this table, so a billable currency is always
// one the webhook can match.
var flexFormCurrencyCodes = map[string]string{
	"AUD": "036",
	"CAD": "124",
	"EUR": "978",
	"GBP": "826",
	"JPY": "392",
	"USD": "840",
}

// UnsupportedCurrencyError reports a price CCBill cannot bill. It is returned
// before any FlexForm URL exists, so before any charge.
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
	code, ok := flexFormCurrencyCodes[strings.ToUpper(strings.TrimSpace(currency))]
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
