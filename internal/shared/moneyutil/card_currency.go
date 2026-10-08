package moneyutil

import "fmt"

// RequireFiatCurrency protects card rails, whose minor-unit amounts and
// settlement records must never be interpreted as native token quantities.
func RequireFiatCurrency(code string) error {
	units, ok := LookupCurrency(code)
	if !ok {
		return fmt.Errorf("money: unknown currency %q", code)
	}
	if units.Kind != "fiat" {
		return fmt.Errorf("currency %s is not supported by card payment rails", units.Code)
	}
	return nil
}
