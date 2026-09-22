//go:build integration

package checkout

import (
	"context"

	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Both observed and accepted purchases take the customer decision mutex before
// the timeline. Holding only the timeline forces the old inverted order into a
// real PostgreSQL deadlock; the common order instead lets both purchases commit.
func TestSaleAndObservedPurchaseDoNotInvertCustomerTimelineLocks(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.payload.Entitlements = map[string]*int{"lock_order_feature": nil}
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"lock_order_feature":null}' WHERE id=$1`, fx.productID)
	require.NoError(t, err)
	ddl := dbtest.SharedSuperuserPGXPool(t)
	params := intents.EnqueueParams{MerchantID: fx.merchantID.UUID(), Provider: "nmi", PspID: fx.payload.Instrument.PSPID, PriceID: &fx.priceID, IntentType: payments.TypeNMISale, Payload: fx.payload, IdempotencyKey: "sale-lock-" + uuid.NewString(), NextAttemptAt: fx.payload.AcceptedAt, Origin: intents.OriginUser}
	operation, err := intents.NewStore(fx.db).Enqueue(fx.ctx, params)
	require.NoError(t, err)
	gate, err := ddl.Begin(fx.ctx)
	require.NoError(t, err)
	defer func() { _ = gate.Rollback(context.Background()) }()
	require.NoError(t, entitlements.LockEntitlementTimeline(fx.ctx, fx.db.NewWithPgxTx(gate).Qx(fx.ctx), fx.userID, "lock_order_feature"))
	var gatePID int32
	require.NoError(t, gate.QueryRow(fx.ctx, `SELECT pg_backend_pid()`).Scan(&gatePID))
	var advisoryKey int64
	require.NoError(t, gate.QueryRow(fx.ctx, `SELECT (classid::bigint<<32)|objid::bigint FROM pg_locks WHERE pid=pg_backend_pid() AND locktype='advisory' AND granted`).Scan(&advisoryKey))
	require.NoError(t, gate.Commit(fx.ctx))
	gate, err = ddl.Begin(fx.ctx)
	require.NoError(t, err)
	require.NoError(t, gen.New(gate).AcquireEntitlementTimelineLock(fx.ctx, advisoryKey))
	require.NoError(t, gate.QueryRow(fx.ctx, `SELECT pg_backend_pid()`).Scan(&gatePID))

	observed := make(chan error, 1)
	scoped := db.WithPSPID(fx.ctx, fx.payload.Instrument.PSPID)
	go func() {
		_, err := fx.purchase.RegisterPurchase(scoped, &payments.RegisterPurchaseRequest{UserID: fx.userID, PriceID: fx.priceID, Rail: "nmi", TransactionID: "observed-lock-" + uuid.NewString(), Amount: fx.payload.Amount, AmountProvided: true, Currency: fx.payload.Currency})
		observed <- err
	}()
	var observedPID int32
	if !assert.Eventually(t, func() bool {
		err := ddl.QueryRow(fx.ctx, `SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND wait_event='advisory' LIMIT 1`, gatePID).Scan(&observedPID)
		return err == nil
	}, 10*time.Second, 10*time.Millisecond) {
		_ = gate.Rollback(context.Background())
		<-observed
		return
	}
	type outcome struct {
		row gen.OpenrailsRailIntent
		err error
	}
	accepted := make(chan outcome, 1)
	go func() { row, err := fx.runner.ExecuteByID(fx.ctx, operation.ID); accepted <- outcome{row, err} }()

	if !assert.Eventually(t, func() bool {
		var waiting int
		err := ddl.QueryRow(fx.ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, observedPID).Scan(&waiting)
		return err == nil && waiting > 0 && fx.gateway.saleCalls.Load() == 1
	}, 10*time.Second, 10*time.Millisecond) {
		_ = gate.Rollback(context.Background())
		<-observed
		<-accepted
		return
	}
	require.NoError(t, gate.Commit(fx.ctx))
	observedErr, result := <-observed, <-accepted
	require.NoError(t, observedErr, "ordinary provider observation must not deadlock on customer")
	require.NoError(t, result.err)
	require.Equal(t, intents.StatusSucceeded, result.row.Status, "accepted purchase must not lose atomic completion to a customer/timeline cycle: %v", result.row.LastFailureReason)
}
