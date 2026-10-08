// Package currency owns the fixed native-unit scales shared by catalog input,
// billing APIs, provider conversions, and generated client metadata.
package currency

import (
	"sort"
	"strings"
)

// Units describes one currency's native and settlement precision.
type Units struct {
	Code          string
	Decimals      int
	MinorDecimals int
	Kind          string // fiat or crypto
}

// NativeShift is the decimal shift from settlement units to native units.
func (u Units) NativeShift() int { return u.Decimals - u.MinorDecimals }

var registry = map[string]Units{
	"USD":  {Code: "USD", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
	"EUR":  {Code: "EUR", Decimals: 6, MinorDecimals: 2, Kind: "fiat"},
	"JPY":  {Code: "JPY", Decimals: 4, MinorDecimals: 0, Kind: "fiat"},
	"SOL":  {Code: "SOL", Decimals: 9, MinorDecimals: 9, Kind: "crypto"},
	"USDC": {Code: "USDC", Decimals: 6, MinorDecimals: 6, Kind: "crypto"},
}

// Lookup returns the registered scale for a currency code, case-insensitively.
func Lookup(code string) (Units, bool) {
	u, ok := registry[strings.ToUpper(strings.TrimSpace(code))]
	return u, ok
}

// List returns a copy of the registry in currency-code order.
func List() []Units {
	all := make([]Units, 0, len(registry))
	for _, u := range registry {
		all = append(all, u)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Code < all[j].Code })
	return all
}
