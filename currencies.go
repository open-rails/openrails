package openrails

import (
	"sort"
	"strings"
)

// CurrencyUnits is one registered currency's scale. Every monetary integer
// OpenRails emits for the currency is in native units (10^Decimals per major
// unit); providers settle in minor units (10^MinorDecimals per major unit).
type CurrencyUnits struct {
	Code          string `json:"code"`
	Decimals      int    `json:"decimals"`
	MinorDecimals int    `json:"minor_decimals"`
}

// NativeShift is the decimal shift from a rail minor unit to native units:
// native = minor * 10^NativeShift.
func (c CurrencyUnits) NativeShift() int { return c.Decimals - c.MinorDecimals }

// currencyRegistry is the system currency table. It is fixed per release, not
// per merchant, and is the single source for the engine's converters, the
// admin console's scale file and GET /v1/currencies.
var currencyRegistry = map[string]CurrencyUnits{
	"USD": {Code: "USD", Decimals: 6, MinorDecimals: 2},
	"EUR": {Code: "EUR", Decimals: 6, MinorDecimals: 2},
	"JPY": {Code: "JPY", Decimals: 4, MinorDecimals: 0},
}

// Currencies lists the registry in code order; no I/O.
func Currencies() []CurrencyUnits {
	out := make([]CurrencyUnits, 0, len(currencyRegistry))
	for _, units := range currencyRegistry {
		out = append(out, units)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// LookupCurrency returns the scale registered for code (case-insensitive).
func LookupCurrency(code string) (CurrencyUnits, bool) {
	units, ok := currencyRegistry[strings.ToUpper(strings.TrimSpace(code))]
	return units, ok
}

// CurrencyRegistry is the GET /v1/currencies document.
type CurrencyRegistry struct {
	Object     string          `json:"object"`
	Currencies []CurrencyUnits `json:"currencies"`
}
