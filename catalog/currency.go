package catalog

import (
	"fmt"

	"github.com/open-rails/openrails/internal/currency"
)

// Currency is a registered currency's code as the registry spells it: USD,
// EUR, SOL. A field holding one is not checked as it decodes; ParseCurrency
// checks one.
type Currency string

// ParseCurrency reads a registered currency code, case-insensitively, and
// returns the registry's spelling.
func ParseCurrency(s string) (Currency, error) {
	units, ok := currency.Lookup(s)
	if !ok {
		return "", fmt.Errorf("unknown currency %q", s)
	}
	return Currency(units.Code), nil
}

// Units is the currency's scale; false when the registry has no such code
// (case-insensitive).
func (c Currency) Units() (CurrencyUnits, bool) {
	units, ok := currency.Lookup(string(c))
	return currencyUnits(units), ok
}

// CurrencyUnits is one registered currency's scale. Every monetary integer
// OpenRails emits for the currency is in native units (10^Decimals per major
// unit); providers settle in minor units (10^MinorDecimals per major unit).
type CurrencyUnits struct {
	Code          Currency `json:"code"`
	Decimals      int      `json:"decimals"`
	MinorDecimals int      `json:"minor_decimals"`
}

// NativeShift is the decimal shift from a rail minor unit to native units:
// native = minor * 10^NativeShift.
func (c CurrencyUnits) NativeShift() int { return c.Decimals - c.MinorDecimals }

// Currencies lists the registry in code order.
func Currencies() []CurrencyUnits {
	registered := currency.List()
	out := make([]CurrencyUnits, 0, len(registered))
	for _, units := range registered {
		out = append(out, currencyUnits(units))
	}
	return out
}

func currencyUnits(u currency.Units) CurrencyUnits {
	return CurrencyUnits{Code: Currency(u.Code), Decimals: u.Decimals, MinorDecimals: u.MinorDecimals}
}
