package checkout

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

var prorationNow = time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

func intPtr(v int) *int { return &v }

func usd(micros int64) PriceAmount { return PriceAmount{Micros: micros, Currency: "USD"} }

// quoteLeft prices an upgrade from a plan whose current period of length
// period has left remaining at prorationNow.
func quoteLeft(old, new PriceAmount, period, left time.Duration, newCycle int) (ModelBUpgradeQuote, error) {
	end := prorationNow.Add(left)
	start := end.Add(-period)
	return QuoteModelBUpgrade(ModelBUpgrade{Old: old, New: new, PeriodStart: &start, PeriodEnd: &end, NewCycleHours: &newCycle}, prorationNow)
}

// #1067: credit is measured against the old plan's own period at sub-second
// precision, whatever the new cadence, and rounds once, up to a whole rail
// minor unit (customer-favoured). Cadence rows that greenfield also drives
// through a real NMI sale are kept only where they pin rounding edges.
func TestModelBUpgradeQuote(t *testing.T) {
	const h = time.Hour
	for _, tc := range []struct {
		name          string
		old, new      int64
		period, left  time.Duration
		newCycle      int
		credit, owing int64
	}{
		{"$20->$50 with 28d of 30d left", 20_000_000, 50_000_000, 720 * h, 672 * h, 720, 18_670_000, 31_330_000},
		{"no time left charges full price", 20_000_000, 50_000_000, 720 * h, 0, 720, 0, 50_000_000},
		{"period already ended", 20_000_000, 50_000_000, 720 * h, -120 * h, 720, 0, 50_000_000},
		{"full period left", 20_000_000, 50_000_000, 720 * h, 720 * h, 720, 20_000_000, 30_000_000},
		{"30m left on 1h", 2_000_000, 5_000_000, h, 30 * time.Minute, 1, 1_000_000, 4_000_000},
		{"1h with 60s left to 720h", 3_600_000, 10_000_000, h, time.Minute, 720, 60_000, 9_940_000},
		{"one second left rounds up to a cent", 10_000_000, 20_000_000, 720 * h, time.Second, 720, 10_000, 19_990_000},
		{"half a second left still credits a cent", 10_000_000, 20_000_000, h, 500 * time.Millisecond, 1, 10_000, 19_990_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := quoteLeft(usd(tc.old), usd(tc.new), tc.period, tc.left, tc.newCycle)
			require.NoError(t, err)
			require.Equal(t, tc.credit, q.Credit)
			require.Equal(t, tc.owing, q.ChargeNow)
			require.Equal(t, prorationNow, q.PeriodStart)
			require.Equal(t, prorationNow.Add(time.Duration(tc.newCycle)*h), q.PeriodEnd)
			_, err = moneyutil.NativeToRailMinorExact("USD", q.ChargeNow)
			require.NoError(t, err, "whole-cent prices yield a whole-cent charge")
		})
	}

	t.Run("clock before period start credits the whole period, never more than paid", func(t *testing.T) {
		start, end := prorationNow.Add(time.Hour), prorationNow.Add(2*time.Hour)
		q, err := QuoteModelBUpgrade(ModelBUpgrade{Old: usd(3_333_333), New: usd(9_000_000), PeriodStart: &start, PeriodEnd: &end, NewCycleHours: intPtr(1)}, prorationNow)
		require.NoError(t, err)
		require.Equal(t, int64(3_333_333), q.Credit, "a sub-cent price is capped at itself, not rounded above it")
		require.Equal(t, int64(5_666_667), q.ChargeNow)
	})

	t.Run("NMI wire amount", func(t *testing.T) {
		q, err := quoteLeft(usd(20_000_000), usd(50_000_000), 720*time.Hour, 672*time.Hour, 720)
		require.NoError(t, err)
		cents, err := moneyutil.NativeToRailMinorExact("USD", q.ChargeNow)
		require.NoError(t, err)
		require.Equal(t, "31.33", moneyutil.FormatCentsDecimal(cents))
	})

	t.Run("credit rounds to the currency's minor unit (JPY)", func(t *testing.T) {
		jpy := func(v int64) PriceAmount { return PriceAmount{Micros: v, Currency: "JPY"} }
		q, err := quoteLeft(jpy(20_000_000), jpy(50_000_000), 720*time.Hour, 672*time.Hour, 720)
		require.NoError(t, err)
		yen, err := moneyutil.NativeToRailMinorExact("JPY", q.ChargeNow)
		require.NoError(t, err)
		require.EqualValues(t, 3133, yen)
	})
}

// amount × period-nanoseconds overflows int64 long before the amounts do; the
// credit must stay exact.
func TestModelBUpgradeQuoteDoesNotOverflow(t *testing.T) {
	const oldFull, newFull int64 = 20_000_000_000_000_000, 30_000_000_000_000_000
	const maxWholeCent = int64(math.MaxInt64 / 10_000 * 10_000)
	for _, tc := range []struct {
		name     string
		old, new int64
		left     time.Duration
		want     int64
	}{
		{"full cycle", oldFull, newFull, 720 * time.Hour, newFull - oldFull},
		{"three quarters left", oldFull, newFull, 540 * time.Hour, 15_000_000_000_000_000},
		{"no remaining time", oldFull, newFull, 0, newFull},
		{"largest whole-cent prices", maxWholeCent - 10_000, maxWholeCent, 720 * time.Hour, 10_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := quoteLeft(usd(tc.old), usd(tc.new), 720*time.Hour, tc.left, 720)
			require.NoError(t, err)
			require.Equal(t, tc.want, q.ChargeNow)
		})
	}
}

// Refusals are typed and produce no amount. Cross-currency is refused in both
// directions (#820): subtracting EUR credit from a USD price invents a 1.0 FX
// rate, undercharging one way and overcharging the other.
func TestModelBUpgradeQuoteRefusals(t *testing.T) {
	start, end := prorationNow.Add(-time.Hour), prorationNow.Add(time.Hour)
	at := func(o, n PriceAmount, s, e *time.Time, cycle *int) ModelBUpgrade {
		return ModelBUpgrade{Old: o, New: n, PeriodStart: s, PeriodEnd: e, NewCycleHours: cycle}
	}
	eur := PriceAmount{Micros: 20_000_000, Currency: "EUR"}
	for _, tc := range []struct {
		name string
		u    ModelBUpgrade
		want error
	}{
		{"eur to usd", at(eur, usd(25_000_000), &start, &end, intPtr(1)), subscriptions.ErrRepriceCrossCurrency},
		{"usd to eur", at(usd(25_000_000), eur, &start, &end, intPtr(1)), subscriptions.ErrRepriceCrossCurrency},
		{"missing old currency", at(PriceAmount{Micros: 1}, usd(2), &start, &end, intPtr(1)), subscriptions.ErrRepriceCrossCurrency},
		{"blank new currency", at(usd(1), PriceAmount{Micros: 2, Currency: "  "}, &start, &end, intPtr(1)), subscriptions.ErrRepriceCrossCurrency},
		{"nil new cycle", at(usd(1), usd(2), &start, &end, nil), ErrTierChangeCycleUnknown},
		{"zero new cycle", at(usd(1), usd(2), &start, &end, intPtr(0)), ErrTierChangeCycleUnknown},
		{"negative new cycle", at(usd(1), usd(2), &start, &end, intPtr(-24)), ErrTierChangeCycleUnknown},
		{"nil period start", at(usd(1), usd(2), nil, &end, intPtr(1)), ErrTierChangePeriodUnknown},
		{"nil period end", at(usd(1), usd(2), &start, nil, intPtr(1)), ErrTierChangePeriodUnknown},
		{"empty period", at(usd(1), usd(2), &end, &end, intPtr(1)), ErrTierChangePeriodUnknown},
		{"inverted period", at(usd(1), usd(2), &end, &start, intPtr(1)), ErrTierChangePeriodUnknown},
		{"long plan early in period is worth more than a short target", at(usd(10_000_000), usd(5_000_000), timePtr(prorationNow.Add(-20*time.Hour)), timePtr(prorationNow.Add(700*time.Hour)), intPtr(168)), ErrTierChangeCreditExceedsPrice},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := QuoteModelBUpgrade(tc.u, prorationNow)
			require.ErrorIs(t, err, tc.want)
			require.Zero(t, q)
			if want, ok := tc.want.(*TierChangeError); ok {
				var tierErr *TierChangeError
				require.True(t, errors.As(err, &tierErr))
				require.Equal(t, want.Code, tierErr.Code)
			}
		})
	}

	t.Run("same currency in any casing computes", func(t *testing.T) {
		q, err := quoteLeft(usd(20_000_000), PriceAmount{Micros: 50_000_000, Currency: " usd "}, 720*time.Hour, 672*time.Hour, 720)
		require.NoError(t, err)
		require.Equal(t, int64(31_330_000), q.ChargeNow)
	})
	t.Run("unregistered currency never prorates", func(t *testing.T) {
		_, err := quoteLeft(PriceAmount{Micros: 1, Currency: "XXX"}, PriceAmount{Micros: 2, Currency: "XXX"}, time.Hour, time.Minute, 1)
		require.Error(t, err)
	})
}
