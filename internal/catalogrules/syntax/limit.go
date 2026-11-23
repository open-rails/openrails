package syntax

import (
	"fmt"
	"regexp"

	"github.com/open-rails/openrails/internal/currency"
)

// balanceLimit is catalog.BalanceLimit, the reserved limit key.
const balanceLimit = "balance"

var (
	limitKeyPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	limitUnitPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// ValidateLimitKey refuses a key that is not a lowercase identifier of at
// most 64 bytes, and the reserved key balance.
func ValidateLimitKey(key string) error {
	if key == balanceLimit {
		return fmt.Errorf("limit key %q is reserved for the prepaid balance and credit line", key)
	}
	if !limitKeyPattern.MatchString(key) {
		return fmt.Errorf("limit key %q must match %s", key, limitKeyPattern)
	}
	return nil
}

// LimitUnitCurrency reports whether a limit's unit is a currency code, as
// the registry writes it.
func LimitUnitCurrency(unit string) bool {
	units, ok := currency.Lookup(unit)
	return ok && units.Code == unit
}

// ValidateLimitUnit refuses a unit that is neither a named unit (lowercase)
// nor a registered currency code.
func ValidateLimitUnit(unit string) error {
	if limitUnitPattern.MatchString(unit) || LimitUnitCurrency(unit) {
		return nil
	}
	return fmt.Errorf("limit unit %q must be a lowercase name matching %s or a currency code", unit, limitUnitPattern)
}
