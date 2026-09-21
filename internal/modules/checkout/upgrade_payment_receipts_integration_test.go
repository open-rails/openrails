//go:build integration

package checkout

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"

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

func TestUpgradeAdmissionRefusesInstrumentMovedAfterPreflight(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	tx, err := fx.db.Pool().Begin(fx.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(fx.ctx) }()
	var method uuid.UUID
	require.NoError(t, tx.QueryRow(fx.ctx, `SELECT id FROM billing.payment_methods WHERE id=$1 FOR UPDATE`, *fx.existingSub.PaymentMethodID).Scan(&method))
	var pid int
	require.NoError(t, tx.QueryRow(fx.ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	result := make(chan error, 1)
	go func() { _, err := fx.upgrade(t); result <- err }()
	require.Eventually(t, func() bool {
		var waiting bool
		err := fx.db.Pool().QueryRow(fx.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
		return err == nil && (waiting || fx.gateway.createCalls.Load() > 0)
	}, 10*time.Second, 10*time.Millisecond)
	_, err = tx.Exec(fx.ctx, `UPDATE billing.payment_methods SET rail_customer_ref=$2 WHERE id=$1`, method, "remapped-before-admission")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(fx.ctx))
	require.Error(t, <-result)
	require.Zero(t, fx.gateway.createCalls.Load(), "a stale frozen instrument cannot create a provider schedule")
	require.Zero(t, fx.gateway.saleCalls.Load())
	require.Zero(t, fx.count(t, `SELECT count(*) FROM billing.rail_intents WHERE idempotency_key=$1`, tierChangeIdempotencyKey(fx.req.IdempotencyKey)))
}

func TestAcceptedUpgradePinsInstrumentAgainstLaterCustodyRemap(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.svc.Intents.(*intents.Runner).Registry = intents.NewRegistry()
	fx.upgradeProcessing(t)
	count, err := fx.db.Gen(fx.ctx).CountUnresolvedOperationsNamingPaymentMethod(fx.ctx, gen.CountUnresolvedOperationsNamingPaymentMethodParams{MerchantID: fx.existingSub.MerchantID, PaymentMethodID: *fx.existingSub.PaymentMethodID})
	require.NoError(t, err)
	require.Positive(t, count, "the real custody-remap predicate must see the accepted upgrade")
	require.Zero(t, fx.gateway.createCalls.Load())
	require.Zero(t, fx.gateway.saleCalls.Load())
}
