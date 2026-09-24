package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBillingCycleFormats(t *testing.T) {
	for hours, want := range map[int]string{1: "1h", 24: "24h", 720: "720h"} {
		require.Equal(t, want, BillingCycleHoursToInterval(hours))
	}
	for days, want := range map[int]struct {
		interval string
		count    int
	}{
		1: {"day", 1}, 7: {"week", 1}, 14: {"week", 2}, 10: {"day", 10},
		28: {"month", 1}, 30: {"month", 1}, 31: {"month", 1}, 60: {"month", 2}, 90: {"month", 3},
		365: {"year", 1}, 366: {"year", 1}, 730: {"year", 2},
	} {
		interval, count := BillingCycleDaysToStripeRecurring(days)
		require.Equal(t, want.interval, interval, days)
		require.Equal(t, want.count, count, days)
	}
}

func TestParseExpiry(t *testing.T) {
	for exp, want := range map[string]struct {
		month, year int
		ok          bool
	}{
		"02/27": {2, 2027, true}, "11-2030": {11, 2030, true}, " 1 / 29 ": {1, 2029, true},
		"bad": {}, "": {}, "00/27": {}, "12/00": {}, "12/27/01": {}, "ab/27": {}, "12/xy": {},
	} {
		month, year, ok := ParseExpiry(exp)
		require.Equal(t, want.ok, ok, exp)
		require.Equal(t, want.month, month, exp)
		require.Equal(t, want.year, year, exp)
	}
}
