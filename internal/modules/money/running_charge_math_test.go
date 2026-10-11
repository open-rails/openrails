package money

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// The exact native units a rate charges: whole seconds, half up, never a
// negative span, and an error rather than a wrapped amount.
func TestRunningChargeTotalPinsTheUnits(t *testing.T) {
	start := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		rate    int64
		elapsed time.Duration
		want    int64
	}{
		{3_600_000, time.Second, 1000},
		{3_600_000, 999 * time.Millisecond, 0},
		{3_600_000, 1999 * time.Millisecond, 1000},
		{1, 1799 * time.Second, 0},
		{1, 1800 * time.Second, 1},
		{1, 5399 * time.Second, 1},
		{1, 5400 * time.Second, 2},
		{1_234_567, 5000 * time.Second, 1_714_676},
		{1_000_001, 1800 * time.Second, 500_001},
		{2_500_000, (3*86_400 + 7) * time.Second, 180_004_861},
		{7, -time.Hour, 0},
	} {
		got, err := RunningChargeTotal(tc.rate, start, start.Add(tc.elapsed))
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "rate %d over %s", tc.rate, tc.elapsed)
	}
	// The fastest rate over a thousand years still fits.
	millennium := time.Unix(start.Unix()+1000*365*86_400, 0)
	got, err := RunningChargeTotal(billing.MaxRunningChargeRatePerHour, start, millennium)
	require.NoError(t, err)
	require.EqualValues(t, 8_760_000_000_000_000_000, got)
	_, err = RunningChargeTotal(math.MaxInt64, start, start.Add(2*time.Hour))
	require.Error(t, err, "an amount past int64 is refused, not wrapped")
}

// The run-out draws lots soonest-expiring first, loses what a lot has left
// when it lapses, counts what is already claimed, and stops at the floor.
func TestFirstInstantAtOrUnder(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	at := func(seconds int64) *time.Time { t := now.Add(time.Duration(seconds) * time.Second); return &t }
	const rate = 3_600_000 // 1000 a second
	for _, tc := range []struct {
		name      string
		available int64
		lots      []creditLot
		rate      int64
		floor     int64
		want      time.Duration
	}{
		{"one lot", 10_000_000, []creditLot{{10_000_000, nil}}, rate, 0, 10_000 * time.Second},
		{"a hold is drawn first", 6_000_000, []creditLot{{10_000_000, nil}}, rate, 0, 6000 * time.Second},
		{"an overdraft reaches below zero", 0, nil, rate, -2_000_000, 2000 * time.Second},
		{"what is owed raises the floor", 5_000_000, []creditLot{{5_000_000, nil}}, rate, 1_000_000, 4000 * time.Second},
		{"a lapsing lot loses its rest", 7_000_000, []creditLot{{2_000_000, at(1000)}, {5_000_000, nil}}, rate, 0, 6000 * time.Second},
		{"the lapse itself is the run-out", 2_000_000, []creditLot{{2_000_000, at(1000)}}, rate, 0, 1000 * time.Second},
		{"a lot used up before it lapses loses nothing", 7_000_000, []creditLot{{2_000_000, at(5000)}, {5_000_000, nil}}, rate, 0, 7000 * time.Second},
		{"a hold comes off the lapsing lot", 5_500_000, []creditLot{{2_000_000, at(1000)}, {5_000_000, nil}}, rate, 0, 5500 * time.Second},
		{"two lapses", 9_000_000, []creditLot{{2_000_000, at(1000)}, {3_000_000, at(2000)}, {4_000_000, nil}}, rate, 0, 6000 * time.Second},
		{"already under", -1, nil, rate, 0, 0},
		{"at the floor", 500, []creditLot{{500, nil}}, rate, 500, 0},
		{"money no lot holds is not counted", 7_000_000, []creditLot{{5_000_000, nil}}, rate, 0, 5000 * time.Second},
		{"rounded up to the microsecond", 1, []creditLot{{1, nil}}, 7, 0, 514_285_715 * time.Microsecond},
		{"no further than the horizon", math.MaxInt64, []creditLot{{math.MaxInt64, nil}}, 1, 0, balanceHorizon},
	} {
		got := firstInstantAtOrUnder(now, tc.available, tc.lots, tc.rate, tc.floor)
		require.Equal(t, tc.want, got.Sub(now), tc.name)
	}
}
