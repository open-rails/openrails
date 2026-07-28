package checkout

import (
	"errors"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

// #820: Model-B proration subtracts the OLD plan's unused value from the NEW
// plan's full price. That subtraction is only meaningful inside one currency —
// across an FX boundary it silently invents an exchange rate of 1.0.
//
// Both directions are wrong in opposite ways, so both are pinned:
//   - €20.00/mo -> $25.00/mo, 2 days into a 30-day cycle: old_unused =
//     ceilToCent(20_000_000*672/720) = 18_670_000 EUR micros, first charge
//     would be $6.33 against ~$20.16 of unused value — UNDERCHARGE.
//   - $25.00/mo -> €20.00/mo the other way round — OVERCHARGE.
//
// Neither is computed at all now: the helper refuses.
func TestModelBUpgradeRefusesCrossCurrency(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	periodEnd := timePtr(now.Add(28 * 24 * time.Hour))
	cycle := intPtr(30 * 24)

	for _, tc := range []struct {
		name string
		old  PriceAmount
		new  PriceAmount
	}{
		{
			name: "eur to usd undercharges",
			old:  PriceAmount{Micros: 20_000_000, Currency: "eur"},
			new:  PriceAmount{Micros: 25_000_000, Currency: "usd"},
		},
		{
			name: "usd to eur overcharges",
			old:  PriceAmount{Micros: 25_000_000, Currency: "usd"},
			new:  PriceAmount{Micros: 20_000_000, Currency: "eur"},
		},
		{
			name: "missing old currency is never defaulted",
			old:  PriceAmount{Micros: 20_000_000, Currency: ""},
			new:  PriceAmount{Micros: 25_000_000, Currency: "usd"},
		},
		{
			name: "missing new currency is never defaulted",
			old:  PriceAmount{Micros: 20_000_000, Currency: "usd"},
			new:  PriceAmount{Micros: 25_000_000, Currency: "  "},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, _, err := CalculateModelBUpgradeCharge(tc.old, tc.new, periodEnd, cycle, now)
			require.Error(t, err)
			require.ErrorIs(t, err, subscriptions.ErrRepriceCrossCurrency)
			require.Zero(t, first, "no amount may be produced from a refused calculation")
		})
	}
}

// Same currency in any casing/whitespace still computes — the guard rejects FX
// crossings, not formatting.
func TestModelBUpgradeAllowsSameCurrency(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	first, cycle, err := CalculateModelBUpgradeCharge(
		PriceAmount{Micros: 20_000_000, Currency: "USD"},
		PriceAmount{Micros: 50_000_000, Currency: " usd "},
		timePtr(now.Add(28*24*time.Hour)),
		intPtr(30*24),
		now,
	)
	require.NoError(t, err)
	require.Equal(t, int64(31_330_000), first)
	require.Equal(t, 30*24, cycle)
	require.False(t, errors.Is(err, subscriptions.ErrRepriceCrossCurrency))
}
