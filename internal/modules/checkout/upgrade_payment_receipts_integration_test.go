//go:build integration

package checkout

import (
	"testing"

	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

func TestNMIUpgradeRequiresExactProrationBeforeCompletion(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.positiveProration()
	fx.gateway.saleMode.Store("ambiguousHidden")
	fx.upgradeProcessing(t)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	expectedAmount := fx.gateway.saleAmount.Load().(string)
	require.Equal(t, "60.00", expectedAmount, "60000000 native USD units reach the real NMI form exactly")
	require.Equal(t, "USD", fx.gateway.saleCurrency.Load())
	require.Equal(t, fx.operation(t).ID.String(), fx.gateway.saleOrder.Load(), "provider order names immutable operation, not a shortened caller key")
	fx.gateway.saleAmount.Store("0.01") // exact provider read contradicts frozen60USD
	fx.gateway.saleVisible.Store(true)
	result := fx.restartAndVerify(t)
	require.Equal(t, intents.StatusUnknownNeedsVerify, result.Status, "a matching order is not proof of the frozen money")
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	old, err := fx.svc.SubscriptionService.GetByID(fx.ctx, fx.existingSub.ID)
	require.NoError(t, err)
	require.Equal(t, "active", string(old.Status))
	fx.gateway.saleAmount.Store(expectedAmount)
	require.Equal(t, intents.StatusSucceeded, fx.restartAndVerify(t).Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}
