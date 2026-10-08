package catalog

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/currency"
)

// parseAmount converts a decimal major-unit amount and an explicit currency
// into the registry's native integer units. It never rounds or converts FX.
func parseAmount(input string) (int64, string, error) {
	parts := strings.Fields(input)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("amount requires a decimal value and currency, for example 9.99 USD")
	}
	units, ok := currency.Lookup(parts[1])
	if !ok {
		return 0, "", fmt.Errorf("unknown currency %q; recognized currencies: %s", parts[1], recognizedCurrencies())
	}
	whole, fraction, point := strings.Cut(parts[0], ".")
	if whole == "" || point && fraction == "" {
		return 0, "", fmt.Errorf("amount must be a non-negative plain decimal")
	}
	for _, digits := range []string{whole, fraction} {
		for _, digit := range digits {
			if digit < '0' || digit > '9' {
				return 0, "", fmt.Errorf("amount must be a non-negative plain decimal")
			}
		}
	}
	if len(fraction) > units.Decimals {
		return 0, "", fmt.Errorf("amount in %s must have at most %d decimal places", units.Code, units.Decimals)
	}
	digits := strings.TrimLeft(whole, "0") + fraction + strings.Repeat("0", units.Decimals-len(fraction))
	if digits == "" {
		digits = "0"
	}
	amount, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("amount exceeds int64 native units for %s", units.Code)
	}
	return amount, units.Code, nil
}

func recognizedCurrencies() string {
	var codes []string
	for _, units := range currency.List() {
		codes = append(codes, units.Code)
	}
	return strings.Join(codes, ", ")
}
