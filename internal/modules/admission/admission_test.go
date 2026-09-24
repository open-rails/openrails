package admission

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Integer math with the division last: a sub-hour window must not round its
// own numerator away ($1 in a minute is $60/hour).
func TestRatePerHour(t *testing.T) {
	const dollar = int64(1_000_000)
	cases := []struct {
		amount int64
		window time.Duration
		want   int64
	}{
		{dollar, time.Hour, dollar},
		{dollar, time.Minute, 60 * dollar},
		{dollar, 30 * time.Minute, 2 * dollar},
		{dollar, 24 * time.Hour, dollar / 24},
		{1, time.Second, 3600},
		{0, time.Minute, 0},
		{-5, time.Hour, 0},
		{dollar, 0, dollar},                      // degenerate window falls back to one hour
		{dollar, 500 * time.Millisecond, dollar}, // sub-second truncates to 0s, same fallback
		{dollar, -time.Hour, dollar},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, RatePerHour(tc.amount, tc.window), "%d over %s", tc.amount, tc.window)
	}
}

func TestDenialKeysRoundTrip(t *testing.T) {
	merchant := "7f0c6d1e-3b1a-4c55-9a52-0e2b7a1f9c10"
	at := time.Date(2026, 9, 24, 13, 47, 12, 0, time.FixedZone("x", 5*3600))
	key := DenialKey(merchant, at)
	gotMerchant, hour, ok := ParseDenialKey(key)
	require.True(t, ok)
	require.Equal(t, merchant, gotMerchant)
	require.True(t, at.UTC().Truncate(time.Hour).Equal(hour), "keys bucket by UTC hour")
	require.Equal(t, key, DenialKey(merchant, at.Add(10*time.Minute)), "same hour, same key")

	for _, bad := range []string{"", "other:" + merchant + ":1", DenialKeyPrefix + "1", DenialKeyPrefix + ":1", DenialKeyPrefix + merchant + ":nope"} {
		_, _, ok := ParseDenialKey(bad)
		require.False(t, ok, "%q", bad)
	}

	customer, reason, ok := ParseDenialField("cust|budget_exceeded")
	require.True(t, ok)
	require.Equal(t, "cust", customer)
	require.Equal(t, "budget_exceeded", reason)
	for _, bad := range []string{"", "cust", "|reason", "cust|"} {
		_, _, ok := ParseDenialField(bad)
		require.False(t, ok, "%q", bad)
	}
}

// Recording is best-effort and must never affect the admission decision.
func TestDenialRecorderWithoutRedisIsNoop(t *testing.T) {
	var nilRecorder *DenialRecorder
	for _, r := range []*DenialRecorder{nilRecorder, NewDenialRecorder(nil)} {
		require.NotPanics(t, func() {
			r.Record(context.Background(), "merchant", "customer", "budget_exceeded", time.Now())
		})
	}
}
