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

// registry is OpenRails' fixed set of recognized currencies. Fiat native units
// are 10^4 per ISO 4217 minor unit; codes whose card-rail unit differs from ISO
// (HUF, ISK, TWD, UGX, three-decimal) are left out. Crypto entries are the
// Solana rail's settlable tokens at on-chain precision.
var registry = func() map[string]Units {
	out := map[string]Units{}
	for _, code := range []string{"AED", "AUD", "BRL", "CAD", "CHF", "CNY", "CZK", "DKK", "EUR", "GBP", "HKD", "ILS", "INR", "MXN", "MYR", "NOK", "NZD", "PHP", "PLN", "SAR", "SEK", "SGD", "THB", "TRY", "USD", "ZAR"} {
		out[code] = Units{Code: code, Decimals: 6, MinorDecimals: 2, Kind: "fiat"}
	}
	for _, code := range []string{"JPY", "KRW"} {
		out[code] = Units{Code: code, Decimals: 4, MinorDecimals: 0, Kind: "fiat"}
	}
	for code, decimals := range map[string]int{"SOL": 9, "USDC": 6, "USDT": 6, "PYUSD": 6, "USD1": 6, "USDG": 6} {
		out[code] = Units{Code: code, Decimals: decimals, MinorDecimals: decimals, Kind: "crypto"}
	}
	return out
}()

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
