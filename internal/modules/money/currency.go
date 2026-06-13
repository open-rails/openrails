package money

import (
	"fmt"
	"strings"
)

// System currency registry (#472). Currency is system-fixed, NOT tenant-scoped:
// the codebase is the authority (there is no DB CHECK). amount columns are
// integers in the currency's minor unit (Decimals places below the major unit).

// DefaultCurrency is assumed wherever a currency is unset ("").
const DefaultCurrency = "USD"

// Currency is a system currency code and its minor-unit scale.
type Currency struct {
	Code     string
	Decimals int    // minor units per major unit = 10^Decimals
	Kind     string // "fiat" | "crypto"
}

var currencies = map[string]Currency{
	"USD":  {Code: "USD", Decimals: 6, Kind: "fiat"}, // µ$
	"USDC": {Code: "USDC", Decimals: 6, Kind: "crypto"},
	"EUR":  {Code: "EUR", Decimals: 6, Kind: "fiat"},
	"JPY":  {Code: "JPY", Decimals: 4, Kind: "fiat"},
	"SOL":  {Code: "SOL", Decimals: 9, Kind: "crypto"},
}

// normalizeCurrency maps "" to the default and upper-cases the code, so built-in
// currency codes are case-insensitive ("usd" == "USD"). Registry keys are upper.
func normalizeCurrency(c string) string {
	c = strings.ToUpper(strings.TrimSpace(c))
	if c == "" {
		return DefaultCurrency
	}
	return c
}

// ValidateCurrency errors on an unknown code (after normalizing "").
func ValidateCurrency(c string) error {
	if _, ok := currencies[normalizeCurrency(c)]; !ok {
		return fmt.Errorf("money: unknown currency %q", c)
	}
	return nil
}

// CurrencyScale returns the currency's minor-unit decimals.
func CurrencyScale(c string) (int, bool) {
	cur, ok := currencies[normalizeCurrency(c)]
	return cur.Decimals, ok
}
