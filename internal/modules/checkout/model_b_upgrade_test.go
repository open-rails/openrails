package checkout

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func intPtr(v int) *int { return &v }

// usd tags a micro amount with its currency for the Model-B helper (#820).
func usd(micros int64) PriceAmount { return PriceAmount{Micros: micros, Currency: "USD"} }

// quoteAt prices an upgrade from a plan whose current period has length
// period and `left` remaining at now, to a plan with newCycle hours.
func quoteAt(old, new PriceAmount, period, left time.Duration, newCycle *int, now time.Time) (ModelBUpgradeQuote, error) {
	end := now.Add(left)
	start := end.Add(-period)
	return QuoteModelBUpgrade(ModelBUpgrade{Old: old, New: new, PeriodStart: &start, PeriodEnd: &end, NewCycleHours: newCycle}, now)
}

// TestQuoteModelBUpgrade pins #1067: the old plan's credit is measured
// against its own current period at sub-second precision, whatever the new
// plan's cadence; money rounds once, up to a whole rail minor unit.
func TestQuoteModelBUpgrade(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	const h = time.Hour
	for _, tc := range []struct {
		name            string
		old, new        int64
		period, left    time.Duration
		newCycle        int
		credit, chargeN int64
	}{
		{"720h $20->$50 with 28d left", 20_000_000, 50_000_000, 720 * h, 672 * h, 720, 18_670_000, 31_330_000},
		{"no time left charges full price", 20_000_000, 50_000_000, 720 * h, 0, 720, 0, 50_000_000},
		{"period already ended", 20_000_000, 50_000_000, 720 * h, -5 * 24 * h, 720, 0, 50_000_000},
		{"full period left", 20_000_000, 50_000_000, 720 * h, 720 * h, 720, 20_000_000, 30_000_000},
		{"30 minutes left on 1h", 2_000_000, 5_000_000, h, 30 * time.Minute, 1, 1_000_000, 4_000_000},
		{"12h left on 1d", 10_000_000, 20_000_000, 24 * h, 12 * h, 24, 5_000_000, 15_000_000},
		{"7d with 84h left to 720h", 5_000_000, 20_000_000, 168 * h, 84 * h, 720, 2_500_000, 17_500_000},
		{"720h with 360h left to 7d", 10_000_000, 20_000_000, 720 * h, 360 * h, 168, 5_000_000, 15_000_000},
		{"90d with 45d left to 365d", 30_000_000, 100_000_000, 90 * 24 * h, 45 * 24 * h, 365 * 24, 15_000_000, 85_000_000},
		{"1h with 60s left to 720h", 3_600_000, 10_000_000, h, time.Minute, 720, 60_000, 9_940_000},
		{"1d with 90min left (sub-hour)", 24_000_000, 30_000_000, 24 * h, 90 * time.Minute, 24, 1_500_000, 28_500_000},
		{"one second left rounds up once", 10_000_000, 20_000_000, 720 * h, time.Second, 720, 10_000, 19_990_000},
		{"half a second left still credits", 10_000_000, 20_000_000, h, 500 * time.Millisecond, 1, 10_000, 19_990_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := quoteAt(usd(tc.old), usd(tc.new), tc.period, tc.left, intPtr(tc.newCycle), now)
			require.NoError(t, err)
			require.Equal(t, tc.credit, q.Credit, "credit")
			require.Equal(t, tc.chargeN, q.ChargeNow, "charge now")
			require.LessOrEqual(t, q.Credit, tc.old, "credit never exceeds the amount paid")
			require.Equal(t, now, q.PeriodStart)
			require.Equal(t, now.Add(time.Duration(tc.newCycle)*h), q.PeriodEnd)
			_, err = moneyutil.NativeToRailMinorExact("USD", q.ChargeNow)
			require.NoError(t, err, "whole-cent prices yield a whole-cent charge")
		})
	}
}

// The period, not now, bounds the credit: a clock before the period start
// credits the whole period and never more than was paid.
func TestQuoteModelBUpgradeCreditNeverExceedsPaid(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	start, end := now.Add(time.Hour), now.Add(2*time.Hour)
	q, err := QuoteModelBUpgrade(ModelBUpgrade{Old: usd(3_333_333), New: usd(9_000_000), PeriodStart: &start, PeriodEnd: &end, NewCycleHours: intPtr(1)}, now)
	require.NoError(t, err)
	require.Equal(t, int64(3_333_333), q.Credit, "a sub-cent price is credited exactly, not rounded above it")
	require.Equal(t, int64(5_666_667), q.ChargeNow)
}

func TestQuoteModelBUpgradeRefusals(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	for _, tc := range []struct {
		name  string
		u     ModelBUpgrade
		want  error
		wantC string
	}{
		{"nil new cycle", ModelBUpgrade{Old: usd(1), New: usd(2), PeriodStart: &start, PeriodEnd: &end}, ErrTierChangeCycleUnknown, "tier_change_cycle_unknown"},
		{"zero new cycle", ModelBUpgrade{Old: usd(1), New: usd(2), PeriodStart: &start, PeriodEnd: &end, NewCycleHours: intPtr(0)}, ErrTierChangeCycleUnknown, "tier_change_cycle_unknown"},
		{"negative new cycle", ModelBUpgrade{Old: usd(1), New: usd(2), PeriodStart: &start, PeriodEnd: &end, NewCycleHours: intPtr(-24)}, ErrTierChangeCycleUnknown, "tier_change_cycle_unknown"},
		{"nil period start", ModelBUpgrade{Old: usd(1), New: usd(2), PeriodEnd: &end, NewCycleHours: intPtr(1)}, ErrTierChangePeriodUnknown, "tier_change_period_unknown"},
		{"nil period end", ModelBUpgrade{Old: usd(1), New: usd(2), PeriodStart: &start, NewCycleHours: intPtr(1)}, ErrTierChangePeriodUnknown, "tier_change_period_unknown"},
		{"empty period", ModelBUpgrade{Old: usd(1), New: usd(2), PeriodStart: &end, PeriodEnd: &end, NewCycleHours: intPtr(1)}, ErrTierChangePeriodUnknown, "tier_change_period_unknown"},
		{"inverted period", ModelBUpgrade{Old: usd(1), New: usd(2), PeriodStart: &end, PeriodEnd: &start, NewCycleHours: intPtr(1)}, ErrTierChangePeriodUnknown, "tier_change_period_unknown"},
		{"credit exceeds new price (720h early -> 7d)", ModelBUpgrade{Old: usd(10_000_000), New: usd(5_000_000), PeriodStart: timePtr(now.Add(-20 * time.Hour)), PeriodEnd: timePtr(now.Add(700 * time.Hour)), NewCycleHours: intPtr(168)}, ErrTierChangeCreditExceedsPrice, "tier_change_credit_exceeds_price"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := QuoteModelBUpgrade(tc.u, now)
			require.ErrorIs(t, err, tc.want)
			var tierErr *TierChangeError
			require.True(t, errors.As(err, &tierErr))
			require.Equal(t, tc.wantC, tierErr.Code)
			require.Zero(t, q, "no amount may be produced from a refused quote")
		})
	}
}

// TestUpgradeWirePinning pins the LITERAL provider amounts of an NMI Model-B
// upgrade (#671): the quote's micros become "31.33" on the sale wire.
func TestUpgradeWirePinning(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	q, err := quoteAt(usd(20_000_000), usd(50_000_000), 720*time.Hour, 672*time.Hour, intPtr(720), now)
	require.NoError(t, err)
	cents, err := moneyutil.NativeToRailMinorExact("USD", q.ChargeNow)
	require.NoError(t, err)
	require.Equal(t, "31.33", moneyutil.FormatCentsDecimal(cents))

	// A price not representable in whole cents errors at the sale seam.
	q, err = quoteAt(usd(20_000_000), usd(50_000_001), 720*time.Hour, 0, intPtr(720), now)
	require.NoError(t, err)
	_, err = moneyutil.NativeToRailMinorExact("USD", q.ChargeNow)
	require.Error(t, err)
}

// TestModelBUpgradeIsCurrencyScaled (or#863): the credit rounds up to a whole
// rail minor unit of the currency (whole yen for JPY), via the registry.
func TestModelBUpgradeIsCurrencyScaled(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	jpy := func(v int64) PriceAmount { return PriceAmount{Micros: v, Currency: "JPY"} }
	q, err := quoteAt(jpy(20_000_000), jpy(50_000_000), 720*time.Hour, 672*time.Hour, intPtr(720), now)
	require.NoError(t, err)
	require.Equal(t, int64(31_330_000), q.ChargeNow)
	yen, err := moneyutil.NativeToRailMinorExact("JPY", q.ChargeNow)
	require.NoError(t, err)
	require.EqualValues(t, 3133, yen)

	_, err = quoteAt(PriceAmount{Micros: 1, Currency: "XXX"}, PriceAmount{Micros: 2, Currency: "XXX"}, time.Hour, time.Minute, intPtr(1), now)
	require.Error(t, err, "an unregistered currency must not prorate")
}
