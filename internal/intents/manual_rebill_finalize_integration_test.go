//go:build integration

package intents

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A confirmed rebill charge is recorded exactly once whatever dunning did to
// the subscription while the outcome was unknown.

// handlerCtx pins what the runner pins before a handler call.
func (fx rebillFixture) handlerCtx() context.Context {
	return db.WithPSPID(merchant.WithID(context.Background(), merchant.ID(fx.merchantID)), fx.pspID)
}

func TestManualRebillRefusalCommitsLifecycleOutboxAndTerminalTogether(t *testing.T) {
	for _, boundary := range []string{"notification", "terminal"} {
		t.Run(boundary, func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			gateway, client := newFakeNMIRebillGateway(t, fx)
			gateway.saleBody.Store("response=2&response_code=202&responsetext=Insufficient+funds")
			ctx := fx.handlerCtx()
			h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
			accepted, err := h.EnqueueScheduled(ctx, fx.subID)
			require.NoError(t, err)
			before := fx.subscription(t)
			var notificationsBefore int
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.notifications WHERE customer_id=$1`, before.CustomerID).Scan(&notificationsBefore))
			admin := dbtest.SharedSuperuserPGXPool(t)
			name := "fail_rebill_" + uuid.New().String()[:8]
			_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION openrails.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected rebill completion failure'; END$$`, name))
			require.NoError(t, err)
			table, event, condition := "notifications", "INSERT", fmt.Sprintf("NEW.customer_id = '%s'::uuid", before.CustomerID)
			if boundary == "terminal" {
				table, event, condition = "rail_intents", "UPDATE", fmt.Sprintf("NEW.id = '%s'::uuid AND NEW.status = 'failed_terminal'", accepted.ID)
			}
			_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON openrails.%s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION openrails.%s()`, name, event, table, condition, name))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS openrails.%s() CASCADE`, name))
			})
			runner := fx.rebillRunner(client, fullModeConfig())
			row, err := runner.ExecuteByID(ctx, accepted.ID)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)
			require.Contains(t, string(row.ResultEvidence), "rebill_decline")
			after := fx.subscription(t)
			require.Equal(t, before.RetryAttempts, after.RetryAttempts)
			require.Equal(t, before.NextRetryAt, after.NextRetryAt)
			var attempts int
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE subscription_id=$1`, fx.subID).Scan(&attempts))
			require.Zero(t, attempts, "local attempt must roll back with failed completion")
			_, err = admin.Exec(ctx, fmt.Sprintf(`DROP FUNCTION openrails.%s() CASCADE`, name))
			require.NoError(t, err)
			for range 2 {
				require.Equal(t, OutcomeTerminal, h.Verify(ctx, row).Class)
			}
			require.EqualValues(t, 2, *fx.subscription(t).RetryAttempts)
			require.Equal(t, StatusFailedTerminal, fx.intentByID(t, row.ID).Status)
			var notificationsAfter int
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.notifications WHERE customer_id=$1`, before.CustomerID).Scan(&notificationsAfter))
			require.Equal(t, notificationsBefore+1, notificationsAfter)
			require.EqualValues(t, 1, gateway.saleCalls.Load(), "local recovery never repeats the provider attempt")
		})
	}
}

func (fx rebillFixture) paymentsFor(t *testing.T, txn string) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2 AND status = 'completed'",
		fx.subID, txn).Scan(&n))
	return n
}

func (fx rebillFixture) entitlementWindows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM openrails.entitlements WHERE source_id = $1 AND source_type = 'subscription' AND revoked_at IS NULL AND (end_at IS NULL OR end_at > now())",
		fx.subID).Scan(&n))
	return n
}

// TestManualRebillConfirmedAfterDunningParkedUnknownRenewsOnce: the charge's
// outcome is lost, dunning parks the subscription as `unknown` meanwhile, and
// the verifier later confirms the sale. The renewal must still land: one
// payment row, period advanced, access granted, status active; a second
// finalize is a no-op.
func TestManualRebillConfirmedAfterDunningParkedUnknownRenewsOnce(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)
	fake.saleStatus.Store(http.StatusBadGateway)
	ctx := context.Background()

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(ctx, fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)

	// Dunning's staleness rule parks the row out of the queue (#839).
	_, err = fx.db.Pool().Exec(ctx, "UPDATE openrails.subscriptions SET status = 'unknown', grace_ends_at = NULL, next_retry_at = NULL, entitlements_spec_snapshot = '{\"premium\": null}'::jsonb WHERE id = $1", fx.subID)
	require.NoError(t, err)

	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(ctx, "UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunVerifyOnce(ctx)
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	require.Equal(t, StatusSucceeded, got.Status)
	sub := fx.subscription(t)
	require.Equal(t, "active", string(sub.Status), "a confirmed charge renews a dunning-parked subscription")
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	require.True(t, sub.CurrentPeriodEndsAt.After(fx.periodEnd), "period advanced from the dunned period end")
	require.Nil(t, sub.GraceEndsAt)
	require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
	require.Equal(t, 1, fx.entitlementWindows(t))
	require.EqualValues(t, 1, fake.saleCalls.Load())

	// A second verification of the same operation (its frozen payload, the
	// same receipt) is idempotent end to end.
	outcome := fx.rebillRunner(client, fullModeConfig()).Registry.Lookup(TypeManualRebill).Verify(fx.handlerCtx(), row)
	require.Equal(t, OutcomeSucceeded, outcome.Class)
	require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
	require.Equal(t, 1, fx.entitlementWindows(t))
	require.True(t, fx.subscription(t).CurrentPeriodEndsAt.Equal(*sub.CurrentPeriodEndsAt), "no second period advance")
	require.EqualValues(t, 1, fake.saleCalls.Load())
}

// TestManualRebillConfirmedOnCancelledSubscriptionRecordsPaymentOnly: the
// customer cancelled while the charge was unknown. The money moved, so the
// payment is recorded once and flagged for refund review; the subscription is
// never reactivated and no access window is granted.
func TestManualRebillConfirmedOnCancelledSubscriptionRecordsPaymentOnly(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)
	fake.saleStatus.Store(http.StatusBadGateway)
	ctx := context.Background()

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(ctx, fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)

	_, err = fx.db.Pool().Exec(ctx, "UPDATE openrails.subscriptions SET status = 'cancelled', cancel_type = 'user', cancelled_at = now(), ended_at = now(), next_retry_at = NULL, grace_ends_at = NULL WHERE id = $1", fx.subID)
	require.NoError(t, err)

	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(ctx, "UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunVerifyOnce(ctx)
	require.NoError(t, err)

	got := fx.intentByID(t, row.ID)
	require.Equal(t, StatusSucceeded, got.Status)
	sub := fx.subscription(t)
	require.Equal(t, "cancelled", string(sub.Status), "a user cancellation is never undone by a late charge")
	require.True(t, sub.CurrentPeriodEndsAt.Equal(fx.periodEnd), "no period advance without reactivation")
	require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
	require.Zero(t, fx.entitlementWindows(t))

	var review string
	require.NoError(t, fx.db.Pool().QueryRow(ctx, "SELECT metadata->>'refund_review' FROM openrails.payments WHERE subscription_id = $1 AND transaction_id = $2", fx.subID, fake.txnID).Scan(&review))
	require.NotEmpty(t, review)

	outcome := fx.rebillRunner(client, fullModeConfig()).Registry.Lookup(TypeManualRebill).Verify(fx.handlerCtx(), row)
	require.Equal(t, OutcomeSucceeded, outcome.Class)
	require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
	require.Equal(t, "cancelled", string(fx.subscription(t).Status))
}
