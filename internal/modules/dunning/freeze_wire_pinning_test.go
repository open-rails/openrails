package dunning

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestFreezeRebillPinsTheRailAmount (MONEY-7 wire pinning): the frozen rebill
// amount is the price in rail minor units, exactly — it is what the exact
// receipt must read back. A price the rail cannot carry exactly is refused
// (the NMI plan push refuses it too), never rounded into an expectation no
// provider sale could match.
func TestFreezeRebillPinsTheRailAmount(t *testing.T) {
	periodEnd := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	psp := uuid.New()
	sub := func(amount int64, currency string) *models.Subscription {
		return &models.Subscription{
			ID: uuid.New(), Rail: models.RailNMI, CurrentPeriodEndsAt: &periodEnd,
			PspID:         psp,
			PaymentMethod: &models.PaymentMethod{ID: uuid.New(), PspID: psp, Custodian: models.CustodianPSP, RailCustomerRef: " vault-1 ", RailMethodRef: "billing-1"},
			Price:         &models.Price{ID: uuid.New(), Amount: amount, Currency: currency},
		}
	}
	for _, tc := range []struct {
		amount   int64
		currency string
		minor    moneyutil.Cents
	}{
		{12_340_000, "USD", 1234},
		{9_223_372_036_850_000, "USD", 922_337_203_685},
		{10_000_000, "JPY", 1_000}, // JPY: 4 internal decimals, 0 minor
	} {
		p, err := FreezeRebill(context.Background(), nil, sub(tc.amount, tc.currency))
		require.NoError(t, err, "%d %s", tc.amount, tc.currency)
		require.Equal(t, tc.minor, p.AmountMinor, "%d %s", tc.amount, tc.currency)
		require.Equal(t, tc.amount, p.Amount)
		require.Equal(t, "vault-1", p.Instrument.RailCustomerRef)
		require.Equal(t, psp, p.Instrument.PSPID)
		require.Equal(t, tc.minor, p.Receipt().Amount, "the receipt expects exactly the frozen minor amount")
	}
	_, err := FreezeRebill(context.Background(), nil, sub(12_345_678, "USD"))
	require.ErrorIs(t, err, ErrNothingToFreeze, "a sub-cent price is refused, never rounded")
}
