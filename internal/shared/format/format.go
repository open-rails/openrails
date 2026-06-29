package format

import (
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

// ParseExpiry parses common MM/YY, MM/YYYY, MM-YY, and MM-YYYY expiry strings.
func ParseExpiry(exp string) (int, int, bool) {
	exp = strings.TrimSpace(exp)
	if exp == "" {
		return 0, 0, false
	}

	sep := "/"
	if strings.Contains(exp, "-") {
		sep = "-"
	}

	parts := strings.Split(exp, sep)
	if len(parts) != 2 {
		return 0, 0, false
	}

	month, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, false
	}
	year, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, false
	}
	if month == 0 || year == 0 {
		return 0, 0, false
	}
	if year < 100 {
		year += 2000
	}

	return month, year, true
}
