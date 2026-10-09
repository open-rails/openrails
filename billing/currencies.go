package billing

import "github.com/open-rails/openrails/internal/currency"

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

// Currencies lists the registry in code order; no I/O.
func Currencies() []CurrencyUnits {
	registered := currency.List()
	out := make([]CurrencyUnits, 0, len(registered))
	for _, units := range registered {
		out = append(out, CurrencyUnits{Code: units.Code, Decimals: units.Decimals, MinorDecimals: units.MinorDecimals})
	}
	return out
}

// LookupCurrency returns the scale registered for code (case-insensitive).
func LookupCurrency(code string) (CurrencyUnits, bool) {
	units, ok := currency.Lookup(code)
	return CurrencyUnits{Code: units.Code, Decimals: units.Decimals, MinorDecimals: units.MinorDecimals}, ok
}
