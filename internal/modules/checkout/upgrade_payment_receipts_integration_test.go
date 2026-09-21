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

func TestUpgradeRequiresItsOwnSuccessorSchedule(t *testing.T) {
	for _, mode := range []string{"approve", "ambiguousLanded"} {
		t.Run(mode, func(t *testing.T) {
			fx := newUpgradeAdoptFixture(t)
			fx.positiveProration()
			fx.gateway.createMode.Store(mode)
			fx.gateway.reportedOrder.Store("another-operation")
			fx.upgradeProcessing(t)
			require.Zero(t, fx.gateway.saleCalls.Load(), "a live schedule on the same vault/plan is not this operation's successor")
			_, err := fx.resolve(t, intents.Resolution{Step: "successor", ProviderReference: fx.gateway.subID, Actor: "operator", Reason: "candidate exact id"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected)
			require.Equal(t, intents.StatusUnknownNeedsVerify, fx.operation(t).Status)

			fx.gateway.reportedOrder.Store("")
			_, err = fx.resolve(t, intents.Resolution{Step: "successor", ProviderReference: fx.gateway.subID, Actor: "operator", Reason: "corrected correlated report"})
			require.NoError(t, err)
			accepted := fx.operation(t)
			enrollment, found, err := intents.LoadNMIEnrollmentReceipt(accepted)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, fx.gateway.subID, enrollment.SubscriptionID())
			require.Equal(t, intents.NMIEnrollmentOrder(accepted), fx.gateway.lastOrder.Load())
			store := intents.NewStore(fx.db)
			require.Error(t, store.RecordProgress(fx.ctx, accepted.ID, map[string]any{"qualified_enrollment": nil}))
			_, err = fx.upgrade(t)
			require.NoError(t, err)
			require.EqualValues(t, 1, fx.gateway.createCalls.Load())
			require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
			require.NoError(t, store.PruneSucceeded(fx.ctx, accepted.ID, nil, false, false))
			_, found, err = intents.LoadNMIEnrollmentReceipt(fx.operation(t))
			require.NoError(t, err)
			require.True(t, found, "generic tombstone pruning cannot erase qualified enrollment custody")
		})
	}
}

func TestFreeUpgradeScheduleDoesNotCreatePaymentAgreement(t *testing.T) {
	fx := newUpgradeAdoptFixture(t)
	fx.gateway.planPayments.Store("12") // finite valid provider plans remain supported
	var before, after string
	query := `SELECT coalesce(stored_credential_recurring_ref,'') FROM billing.payment_methods WHERE id=$1`
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, query, *fx.existingSub.PaymentMethodID).Scan(&before))
	_, err := fx.upgrade(t)
	require.NoError(t, err)
	require.Zero(t, fx.gateway.saleCalls.Load())
	require.NoError(t, fx.db.Qx(fx.ctx).QueryRow(fx.ctx, query, *fx.existingSub.PaymentMethodID).Scan(&after))
	require.Equal(t, before, after, "a delayed enrollment's transactionid is its schedule id, not a paid CIT")
	_, found, err := intents.LoadCollectedReceipt(fx.operation(t))
	require.NoError(t, err)
	require.False(t, found)
	_, found, err = intents.LoadNMIEnrollmentReceipt(fx.operation(t))
	require.NoError(t, err)
	require.True(t, found)
}

func TestSuccessorReceiptRefusesChangedCommercialTerms(t *testing.T) {
	for _, field := range []string{"amount", "date", "vault", "count missing", "count negative", "count malformed"} {
		t.Run(field, func(t *testing.T) {
			fx := newUpgradeAdoptFixture(t)
			fx.positiveProration()
			fx.gateway.createMode.Store("ambiguousLanded")
			fx.upgradeProcessing(t)
			switch field {
			case "amount":
				fx.gateway.recurringAmount.Store("0.01")
			case "date":
				fx.gateway.nextChargeDate.Store("2030-01-01")
			case "vault":
				fx.gateway.railCustomerRef = "another-card"
			case "count missing":
				fx.gateway.planPayments.Store("")
			case "count negative":
				fx.gateway.planPayments.Store("-1")
			case "count malformed":
				fx.gateway.planPayments.Store("unknown")
			}
			_, err := fx.resolve(t, intents.Resolution{Step: "successor", ProviderReference: fx.gateway.subID, Actor: "operator", Reason: "candidate with changed terms"})
			require.ErrorIs(t, err, intents.ErrResolutionRejected)
			require.Equal(t, intents.StatusUnknownNeedsVerify, fx.operation(t).Status)
			require.Zero(t, fx.gateway.saleCalls.Load())
		})
	}
}
