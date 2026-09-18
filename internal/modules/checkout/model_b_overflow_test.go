package checkout_test

import (
	"math"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/checkout"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	manifest "github.com/open-rails/openrails/pkg/catalog"
	"github.com/stretchr/testify/require"
)

// Catalog amounts may fit int64 while the intermediate amount * hours does not.
// The final credit must remain exact and retain the existing rounding policy.
func TestModelBCreditDoesNotOverflow(t *testing.T) {
	const oldFull int64 = 20_000_000_000_000_000
	const newFull int64 = 30_000_000_000_000_000
	oldRank, newRank := 1, 2
	catalog := manifest.Manifest{
		Version: manifest.SupportedVersion,
		Products: []manifest.Product{
			{Key: "old", DisplayName: "Old", TierGroup: "plans", TierRank: &oldRank,
				Prices: []manifest.Price{{Currency: "USD", UnitAmount: oldFull, Duration: "30d", AutoRenew: true, PSPs: []string{"solana"}}}},
			{Key: "new", DisplayName: "New", TierGroup: "plans", TierRank: &newRank,
				Prices: []manifest.Price{{Currency: "USD", UnitAmount: newFull, Duration: "30d", AutoRenew: true, PSPs: []string{"solana"}}}},
		},
	}
	require.NoError(t, catalog.Validate(), "the actual catalog validator accepts these tier/price terms")
	for _, amount := range []int64{oldFull, newFull} {
		_, err := moneyutil.NativeToRailMinorExact("USD", amount)
		require.NoError(t, err, "both amounts are exactly representable in rail minor units")
	}
	now := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	cycle := 720
	const maxWholeCent = int64(math.MaxInt64 / 10_000 * 10_000)
	for _, tc := range []struct {
		name     string
		old, new int64
		hours    int
		want     int64
	}{
		{"full cycle", oldFull, newFull, 720, newFull - oldFull},
		{"partial cycle", oldFull, newFull, 540, 15_000_000_000_000_000},
		{"clamped cycle", oldFull, newFull, 1440, newFull - oldFull},
		{"no remaining time", oldFull, newFull, 0, newFull},
		{"largest whole-cent prices", maxWholeCent - 10_000, maxWholeCent, 720, 10_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			end := now.Add(time.Duration(tc.hours) * time.Hour)
			got, gotCycle, err := checkout.CalculateModelBUpgradeCharge(
				checkout.PriceAmount{Micros: tc.old, Currency: "USD"},
				checkout.PriceAmount{Micros: tc.new, Currency: "USD"}, &end, &cycle, now)
			require.NoError(t, err)
			require.Equal(t, cycle, gotCycle)
			require.Equal(t, tc.want, got)
		})
	}
}
