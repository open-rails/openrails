//go:build integration

package intents

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestDurablePaymentMethodDeleteClearsCollectionDefaults(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	q := fx.db.Gen(fx.ctx)
	now := time.Now().UTC()
	pm := fx.pm.ID
	for _, currency := range []string{"USD", "EUR"} {
		require.NoError(t, q.InsertMoneyAccountSettingsIfAbsent(fx.ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: fx.pm.CustomerID, Currency: currency, BillingMode: "prepaid", Now: now}))
		n, err := q.SetMoneyAccountCollectionPaymentMethod(fx.ctx, gen.SetMoneyAccountCollectionPaymentMethodParams{MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: fx.pm.CustomerID, Currency: currency, PaymentMethodID: &pm, Now: now})
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
	}
	require.True(t, fx.executeThrough(t).Done)
	require.False(t, fx.localRowExists(t))
	for _, currency := range []string{"USD", "EUR"} {
		row, err := fx.db.Gen(fx.ctx).GetMoneyAccountSettings(fx.ctx, gen.GetMoneyAccountSettingsParams{MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: fx.pm.CustomerID, Currency: currency})
		require.NoError(t, err)
		require.Nil(t, row.CollectionPaymentMethodID)
	}
	require.True(t, fx.executeThrough(t).Done)
	require.EqualValues(t, 1, fx.gateway.vaultDeleteCalls.Load(), "replay does not send another delete")
}
