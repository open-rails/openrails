package format

import (
	"errors"
	"strconv"
	"strings"
)

// BillingCycleHoursToInterval formats billing cycle hours as an OpenRails duration.
func BillingCycleHoursToInterval(hours int) string {
	return strconv.Itoa(hours) + "h"
}

// BillingCycleDaysToStripeRecurring converts billing cycle days to Stripe's
// recurring interval shape.
func BillingCycleDaysToStripeRecurring(days int) (string, int) {
	switch {
	case days == 1:
		return "day", 1
	case days == 7:
		return "week", 1
	case days >= 28 && days <= 31:
		return "month", 1
	case days >= 365 && days <= 366:
		return "year", 1
	case days%365 == 0:
		return "year", days / 365
	case days%30 == 0:
		return "month", days / 30
	case days%7 == 0:
		return "week", days / 7
	default:
		return "day", days
	}
}

// ErrInvalidExpiry reports a card expiry that is not MM/YY or MM/YYYY
// (or with "-") with month 1-12 and a year in 2001-2099.
var ErrInvalidExpiry = errors.New("invalid card expiry")

// ParseExpiry parses MM/YY, MM/YYYY, MM-YY, and MM-YYYY expiry strings.
func ParseExpiry(exp string) (month, year int, err error) {
	sep := "/"
	if strings.Contains(exp, "-") {
		sep = "-"
	}
	m, y, ok := strings.Cut(strings.TrimSpace(exp), sep)
	m, y = strings.TrimSpace(m), strings.TrimSpace(y)
	if !ok || len(m) < 1 || len(m) > 2 || (len(y) != 2 && len(y) != 4) || !digits(m) || !digits(y) {
		return 0, 0, ErrInvalidExpiry
	}
	month, _ = strconv.Atoi(m)
	year, _ = strconv.Atoi(y)
	if len(y) == 2 {
		year += 2000
	}
	if month < 1 || month > 12 || year < 2001 || year > 2099 {
		return 0, 0, ErrInvalidExpiry
	}
	return month, year, nil
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
