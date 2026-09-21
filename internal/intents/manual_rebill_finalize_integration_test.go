//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A confirmed rebill charge is recorded exactly once whatever dunning did to
// the subscription while the outcome was unknown.

// handlerCtx pins what the runner pins before a handler call.
func (fx rebillFixture) handlerCtx() context.Context {
	return db.WithPSPID(merchant.WithID(context.Background(), merchant.ID(fx.merchantID)), fx.pspID)
}

func TestManualRebillAdmissionReusesOneUnresolvedOperation(t *testing.T) {
	fx := seedPastDueSubscription(t)
	gateway, client := newFakeNMIRebillGateway(t, fx)
	gateway.saleStatus.Store(http.StatusBadGateway)
	ctx := fx.handlerCtx()
	h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	start := make(chan struct{})
	type result struct {
		id  uuid.UUID
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { <-start; row, err := h.EnqueueScheduled(ctx, fx.subID); results <- result{row.ID, err} }()
	}
	close(start)
	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.Equal(t, first.id, second.id)
	row, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(ctx, first.id)
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	replayed, err := h.EnqueueScheduled(ctx, fx.subID)
	require.NoError(t, err)
	require.Equal(t, row.ID, replayed.ID)
	require.JSONEq(t, string(row.Payload), string(replayed.Payload))
	require.EqualValues(t, 1, gateway.saleCalls.Load())
	gateway.charged.Store(true)
	require.Equal(t, OutcomeSucceeded, h.Verify(ctx, row).Class)
	require.EqualValues(t, 1, gateway.saleCalls.Load())
}

func TestManualRebillPaidCompletionRollsBackAndReplaysOffline(t *testing.T) {
	for _, later := range []bool{false, true} {
		t.Run(fmt.Sprintf("later_renewal_%t", later), func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			gateway, client := newFakeNMIRebillGateway(t, fx)
			ctx := fx.handlerCtx()
			h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
			accepted, err := h.EnqueueScheduled(ctx, fx.subID)
			require.NoError(t, err)
			admin := dbtest.SharedSuperuserPGXPool(t)
			name := "fail_rebill_paid_" + uuid.NewString()[:8]
			_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected paid terminal failure'; END$$;
CREATE TRIGGER %s BEFORE UPDATE ON billing.rail_intents FOR EACH ROW WHEN (NEW.id='%s'::uuid AND NEW.status='succeeded') EXECUTE FUNCTION billing.%s()`, name, name, accepted.ID, name))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS billing.%s() CASCADE`, name))
			})
			row, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(ctx, accepted.ID)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)
			_, found, err := LoadCollectedReceipt(row)
			require.NoError(t, err)
			require.True(t, found)
			require.Zero(t, fx.paymentsFor(t, gateway.txnID))
			require.Equal(t, "past_due", string(fx.subscription(t).Status))
			require.True(t, fx.subscription(t).CurrentPeriodEndsAt.Equal(fx.periodEnd))
			_, err = admin.Exec(ctx, fmt.Sprintf(`DROP FUNCTION billing.%s() CASCADE`, name))
			require.NoError(t, err)
			if later {
				p, err := subscriptions.DecodeManualRebillPayload(row)
				require.NoError(t, err)
				laterStart, laterEnd := p.Renewal.PeriodEnd, p.Renewal.PeriodEnd.Add(30*24*time.Hour)
				// The later renewal has different current commercial terms;
				// old receipt completion must retain its original facts without
				// restoring its earlier benefit snapshot over this one.
				_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.subscriptions SET entitlements_spec_snapshot='{"later-benefit":null}' WHERE id=$1`, fx.subID)
				require.NoError(t, err)
				_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.prices SET amount=12990000 WHERE id=$1`, p.Renewal.PriceID)
				require.NoError(t, err)
				require.NoError(t, h.lifecycle(fx.db).RenewMembership(ctx, &subscriptions.RenewMembershipParams{
					Rail: models.RailNMI, RailSubscriptionID: p.RailSubscriptionID, TransactionID: "later-" + uuid.NewString(),
					Amount: 12990000, AmountProvided: true, Currency: p.Renewal.Currency,
					CurrentPeriodStartsAt: &laterStart, CurrentPeriodEndsAt: &laterEnd,
				}))
				require.True(t, fx.subscription(t).CurrentPeriodEndsAt.Equal(laterEnd))
			}
			expected := fx.subscription(t)
			windows := fx.entitlementWindows(t)
			queries := gateway.queryCalls.Load()
			gateway.charged.Store(false)
			for range 2 {
				require.Equal(t, OutcomeSucceeded, h.Verify(ctx, row).Class)
			}
			require.Equal(t, 1, fx.paymentsFor(t, gateway.txnID))
			var paidAmount int64
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT amount FROM billing.payments WHERE subscription_id=$1 AND transaction_id=$2`, fx.subID, gateway.txnID).Scan(&paidAmount))
			require.EqualValues(t, 9990000, paidAmount)
			if later {
				current := fx.subscription(t)
				require.Equal(t, expected.Status, current.Status)
				require.True(t, current.CurrentPeriodStartsAt.Equal(*expected.CurrentPeriodStartsAt))
				require.True(t, current.CurrentPeriodEndsAt.Equal(*expected.CurrentPeriodEndsAt))
				require.Equal(t, expected.PriceID, current.PriceID)
				require.Equal(t, expected.EntitlementsSpecSnapshot, current.EntitlementsSpecSnapshot)
				require.Equal(t, windows, fx.entitlementWindows(t), "old completion cannot extend the later benefit window")
				var metadata string
				require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT COALESCE(metadata->>'refund_review','') FROM billing.payments WHERE subscription_id=$1 AND transaction_id=$2`, fx.subID, gateway.txnID).Scan(&metadata))
				require.Empty(t, metadata, "a late financial record alone is not a cancelled-subscription refund")
			} else {
				require.Equal(t, 1, fx.entitlementWindows(t))
			}
			require.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
			require.Equal(t, queries, gateway.queryCalls.Load(), "local recovery uses retained facts")
			require.EqualValues(t, 1, gateway.saleCalls.Load())
		})
	}
}

func TestManualRebillPreparesAcceptedPriceOnceBeforeCharging(t *testing.T) {
	fx := seedPastDueSubscription(t)
	ctx := fx.handlerCtx()
	target := uuid.New()
	_, err := fx.db.Pool().Exec(ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,access_duration_hours,auto_renew,key) VALUES($1,$2,$3,8000000,'USD',720,true,$4)`, target, fx.merchantID, fx.payload.Renewal.ProductID, "target-"+target.String())
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(ctx, `INSERT INTO billing.subscription_reprices(id,merchant_id,subscription_id,from_price_id,to_price_id,effective_at,status,kind) VALUES($1,$2,$3,$4,$5,$6,'scheduled','reprice')`, uuid.New(), fx.merchantID, fx.subID, fx.payload.Renewal.PriceID, target, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	gateway, client := newFakeNMIRebillGateway(t, fx)
	gateway.loseUpdateResponse.Store(true)
	h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	accepted, err := h.EnqueueScheduled(ctx, fx.subID)
	require.NoError(t, err)
	runner := fx.rebillRunner(client, fullModeConfig())
	row, err := runner.ExecuteByID(ctx, accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusPending, row.Status, "an uncertain idempotent setup write has not submitted money")
	require.Zero(t, gateway.saleCalls.Load())
	require.EqualValues(t, 1, gateway.updateCalls.Load())
	preparation, found, err := loadRebillPreparation(row)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "9.99", preparation.Amount, "preconditions remain the first observed provider state")
	form := gateway.updateForm.Load().(url.Values)
	require.Equal(t, "8.00", form.Get("plan_amount"))
	require.Equal(t, fx.payload.RailSubscriptionID, form.Get("subscription_id"))
	require.Empty(t, form.Get("plan_id"), "preparation must not mutate a shared plan")
	_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.rail_intents SET next_attempt_at=now() WHERE id=$1`, accepted.ID)
	require.NoError(t, err)
	row, err = runner.ExecuteByID(ctx, accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, row.Status)
	require.EqualValues(t, 1, gateway.updateCalls.Load(), "readback confirms the first setup write; retry does not replace its preconditions")
	require.EqualValues(t, 1, gateway.saleCalls.Load())
	retained, found, err := LoadCollectedReceipt(row)
	require.NoError(t, err)
	require.True(t, found)
	require.EqualValues(t, 800, retained.data.NMI.Amount)
	var amount int64
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT amount FROM billing.payments WHERE transaction_id=$1 AND psp_id=$2`, gateway.txnID, fx.pspID).Scan(&amount))
	require.EqualValues(t, 8_000_000, amount)
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
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.notifications WHERE customer_id=$1`, before.CustomerID).Scan(&notificationsBefore))
			admin := dbtest.SharedSuperuserPGXPool(t)
			name := "fail_rebill_" + uuid.New().String()[:8]
			_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected rebill completion failure'; END$$`, name))
			require.NoError(t, err)
			table, event, condition := "notifications", "INSERT", fmt.Sprintf("NEW.customer_id = '%s'::uuid", before.CustomerID)
			if boundary == "terminal" {
				table, event, condition = "rail_intents", "UPDATE", fmt.Sprintf("NEW.id = '%s'::uuid AND NEW.status = 'failed_terminal'", accepted.ID)
			}
			_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON billing.%s FOR EACH ROW WHEN (%s) EXECUTE FUNCTION billing.%s()`, name, event, table, condition, name))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = admin.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS billing.%s() CASCADE`, name))
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
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, fx.subID).Scan(&attempts))
			require.Zero(t, attempts, "local attempt must roll back with failed completion")
			_, err = admin.Exec(ctx, fmt.Sprintf(`DROP FUNCTION billing.%s() CASCADE`, name))
			require.NoError(t, err)
			for range 2 {
				require.Equal(t, OutcomeTerminal, h.Verify(ctx, row).Class)
			}
			require.EqualValues(t, 2, *fx.subscription(t).RetryAttempts)
			require.Equal(t, StatusFailedTerminal, fx.intentByID(t, row.ID).Status)
			var notificationsAfter int
			require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.notifications WHERE customer_id=$1`, before.CustomerID).Scan(&notificationsAfter))
			require.Equal(t, notificationsBefore+1, notificationsAfter)
			require.EqualValues(t, 1, gateway.saleCalls.Load(), "local recovery never repeats the provider attempt")
		})
	}
}

func (fx rebillFixture) paymentsFor(t *testing.T, txn string) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM billing.payments WHERE subscription_id = $1 AND transaction_id = $2 AND status = 'completed'",
		fx.subID, txn).Scan(&n))
	return n
}

func (fx rebillFixture) entitlementWindows(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM billing.entitlements WHERE source_id = $1 AND source_type = 'subscription' AND revoked_at IS NULL AND (end_at IS NULL OR end_at > now())",
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
	_, err = fx.db.Pool().Exec(ctx, "UPDATE billing.subscriptions SET status = 'unknown', grace_ends_at = NULL, next_retry_at = NULL, entitlements_spec_snapshot = '{\"premium\": null}'::jsonb WHERE id = $1", fx.subID)
	require.NoError(t, err)

	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(ctx, "UPDATE billing.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
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
	outcome := fx.rebillRunner(client, fullModeConfig()).Registry.Lookup(subscriptions.TypeManualRebill).Verify(fx.handlerCtx(), row)
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

	_, err = fx.db.Pool().Exec(ctx, "UPDATE billing.subscriptions SET status = 'cancelled', cancel_type = 'user', cancelled_at = now(), ended_at = now(), next_retry_at = NULL, grace_ends_at = NULL WHERE id = $1", fx.subID)
	require.NoError(t, err)

	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(ctx, "UPDATE billing.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
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
	require.NoError(t, fx.db.Pool().QueryRow(ctx, "SELECT metadata->>'refund_review' FROM billing.payments WHERE subscription_id = $1 AND transaction_id = $2", fx.subID, fake.txnID).Scan(&review))
	require.NotEmpty(t, review)

	outcome := fx.rebillRunner(client, fullModeConfig()).Registry.Lookup(subscriptions.TypeManualRebill).Verify(fx.handlerCtx(), row)
	require.Equal(t, OutcomeSucceeded, outcome.Class)
	require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
	require.Equal(t, "cancelled", string(fx.subscription(t).Status))
}
