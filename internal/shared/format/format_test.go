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
	for exp, want := range map[string][2]int{
		"02/27": {2, 2027}, "11-2030": {11, 2030}, " 1 / 29 ": {1, 2029}, "12/2099": {12, 2099},
	} {
		month, year, err := ParseExpiry(exp)
		require.NoError(t, err, exp)
		require.Equal(t, want, [2]int{month, year}, exp)
	}
	for _, exp := range []string{
		"bad", "", "00/27", "13/27", "99/27", "12/00", "12/27/01", "ab/27", "12/xy",
		"12/270", "12/20270", "1/2", "123/27", "+1/27", "-1/27", "12/+27", "12/1999", "01/2000", "12/2100", "12/27-01",
	} {
		month, year, err := ParseExpiry(exp)
		require.ErrorIs(t, err, ErrInvalidExpiry, exp)
		require.Zero(t, month+year, exp)
	}
}
