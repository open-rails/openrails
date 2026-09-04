//go:build integration

package intents

import (
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDurablePaymentMethodDeleteClearsCollectionDefaults(t *testing.T) {
	fx := newVaultDeleteFixture(t)
	svc := money.NewMoneyService(fx.db)
	payer := identity.CustomerID(fx.pm.CustomerID)
	for _, currency := range []string{"USD", "EUR"} {
		require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(fx.ctx, payer, currency, fx.pm.ID))
	}
	defaults, err := svc.CollectionPaymentMethodCurrencies(fx.ctx, payer)
	require.NoError(t, err)
	require.Equal(t, []string{"EUR", "USD"}, defaults[fx.pm.ID])
	require.True(t, fx.executeThrough(t).Done)
	require.False(t, fx.localRowExists(t))
	defaults, err = svc.CollectionPaymentMethodCurrencies(fx.ctx, payer)
	require.NoError(t, err)
	require.Empty(t, defaults)
	for _, currency := range []string{"USD", "EUR"} {
		row, err := fx.db.Gen(fx.ctx).GetMoneyAccountSettings(fx.ctx, gen.GetMoneyAccountSettingsParams{MerchantID: dbtest.TestMerchantID.UUID(), CustomerID: fx.pm.CustomerID, Currency: currency})
		require.NoError(t, err)
		require.Nil(t, row.CollectionPaymentMethodID)
	}
	require.True(t, fx.executeThrough(t).Done)
	require.EqualValues(t, 1, fx.gateway.vaultDeleteCalls.Load(), "replay does not send another delete")
}
