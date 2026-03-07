package format

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBillingCycleDaysToInterval(t *testing.T) {
	tests := []struct {
		name     string
		days     int
		interval string
		count    int
	}{
		{name: "daily", days: 1, interval: "day", count: 1},
		{name: "weekly", days: 7, interval: "week", count: 1},
		{name: "monthly", days: 30, interval: "month", count: 1},
		{name: "yearly", days: 365, interval: "year", count: 1},
		{name: "biweekly", days: 14, interval: "week", count: 2},
		{name: "fallback days", days: 10, interval: "day", count: 10},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			interval, count := BillingCycleDaysToInterval(tt.days)
			require.Equal(t, tt.interval, interval)
			require.Equal(t, tt.count, count)
		})
	}
}

func TestParseExpiry(t *testing.T) {
	tests := []struct {
		name  string
		exp   string
		month int
		year  int
		ok    bool
	}{
		{name: "short slash", exp: "02/27", month: 2, year: 2027, ok: true},
		{name: "long dash", exp: "11-2030", month: 11, year: 2030, ok: true},
		{name: "invalid", exp: "bad", ok: false},
		{name: "empty", exp: "", ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			month, year, ok := ParseExpiry(tt.exp)
			require.Equal(t, tt.ok, ok)
			require.Equal(t, tt.month, month)
			require.Equal(t, tt.year, year)
		})
	}
}
