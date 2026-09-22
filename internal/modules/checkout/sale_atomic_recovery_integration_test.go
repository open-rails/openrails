//go:build integration

package checkout

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func TestSaleReceiptAndLocalFailuresRecoverExactlyOnce(t *testing.T) {
	for _, boundary := range []string{"receipt", "terminal", "outbox", "anchor"} {
		t.Run(boundary, func(t *testing.T) {
			fx := newSaleIntentFixture(t)
			fx.payload.Entitlements = map[string]*int{"atomic_sale_benefit": nil}
			ddl := dbtest.SharedSuperuserPGXPool(t)
			name := "sale_failure_" + uuid.NewString()[:8]
			var create, drop string
			switch boundary {
			case "receipt", "terminal":
				predicate := fmt.Sprintf("NEW.price_id='%s'::uuid AND NEW.intent_type='nmi_sale'", fx.priceID)
				if boundary == "receipt" {
					predicate += " AND NEW.result_evidence ? 'qualified_receipt'"
				} else {
					predicate += " AND NEW.status='succeeded'"
				}
				create = fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected sale boundary failure'; END$$; CREATE TRIGGER %s BEFORE UPDATE ON billing.rail_intents FOR EACH ROW WHEN (%s) EXECUTE FUNCTION billing.%s()`, name, name, predicate, name)
				drop = "DROP FUNCTION IF EXISTS billing." + name + "() CASCADE"
			case "outbox":
				create = fmt.Sprintf(`ALTER TABLE billing.host_outbox ADD CONSTRAINT %s CHECK(payment_id<>'%s'::uuid)`, name, fx.payload.PaymentID)
				drop = "ALTER TABLE billing.host_outbox DROP CONSTRAINT IF EXISTS " + name
			case "anchor":
				create = fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected sale agreement failure'; END$$; CREATE TRIGGER %s BEFORE UPDATE ON billing.payment_methods FOR EACH ROW WHEN (NEW.id='%s'::uuid AND NEW.stored_credential_unscheduled_ref IS DISTINCT FROM OLD.stored_credential_unscheduled_ref) EXECUTE FUNCTION billing.%s()`, name, name, fx.payload.PaymentMethodID, name)
				drop = "DROP FUNCTION IF EXISTS billing." + name + "() CASCADE"
			}
			_, err := ddl.Exec(fx.ctx, create)
			require.NoError(t, err)
			remove := func() { _, err := ddl.Exec(context.Background(), drop); require.NoError(t, err) }
			t.Cleanup(remove)
			row := fx.enqueueAndExecute(t, uuid.NewString())
			require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
			require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
			_, retained, err := intents.LoadCollectedReceipt(row)
			require.NoError(t, err)
			require.Equal(t, boundary != "receipt", retained)
			require.Zero(t, fx.paymentCount(t))
			var count int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.grants WHERE customer_id=$1`, fx.customerID).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.host_outbox WHERE payment_id=$1`, fx.payload.PaymentID).Scan(&count))
			require.Zero(t, count)
			require.Empty(t, fx.unscheduledRef(t, fx.payload.PaymentMethodID))
			remove()
			beforeReads := fx.gateway.queryCalls.Load()
			if retained {
				fx.gateway.unavailable.Store(true)
			}
			scoped := db.WithPSPID(fx.ctx, *row.PspID)
			handler := fx.runner.Registry.Lookup(payments.TypeNMISale)
			start := make(chan struct{})
			results := make(chan intents.Outcome, 2)
			for range 2 {
				go func() { <-start; results <- handler.Verify(scoped, row) }()
			}
			close(start)
			for range 2 {
				out := <-results
				require.Equal(t, intents.OutcomeSucceeded, out.Class, out.Reason)
			}
			require.Equal(t, 1, fx.paymentCount(t))
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.grants WHERE customer_id=$1 AND event='grant'`, fx.customerID).Scan(&count))
			require.Equal(t, 2, count)
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.host_outbox WHERE payment_id=$1`, fx.payload.PaymentID).Scan(&count))
			require.Equal(t, 1, count)
			require.Equal(t, fx.gateway.txnID, fx.unscheduledRef(t, fx.payload.PaymentMethodID))
			require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
			if retained {
				require.Equal(t, beforeReads, fx.gateway.queryCalls.Load(), "durable custody completes without provider access")
			}
		})
	}
}

func TestSaleStaleExecutorCannotReleaseSubmittedChargeAsUnsent(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("ambiguous500")
	fx.gateway.hidden.Store(true)
	row := fx.enqueueAndExecute(t, uuid.NewString())
	require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
	require.EqualValues(t, 1, row.Attempts)
	// A different approved interaction can establish an agreement while this
	// initial sale is uncertain. The old invocation must consult durable progress.
	fx.seedSaleInstrument(t, "another-approved-agreement")
	handler := fx.runner.Registry.Lookup(payments.TypeNMISale)
	outcome := handler.Execute(db.WithPSPID(fx.ctx, *row.PspID), row)
	require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
	current, err := intents.NewStore(fx.db).Get(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
}

func TestSaleUnsentRecoveryDispatchesOnceThroughWriteGates(t *testing.T) {
	for _, crash := range []bool{false, true} {
		name := "read_only_park"
		if crash {
			name = "claim_lost_before_submission"
		}
		t.Run(name, func(t *testing.T) {
			fx := newSaleIntentFixture(t)
			handler := fx.runner.Registry.Lookup(payments.TypeNMISale).(*NMISaleIntentHandler)
			client, err := handler.Sale.ResolveNMIClient(fx.ctx, "mobius")
			require.NoError(t, err)
			client.ReadOnly = true
			row := fx.enqueueAndExecute(t, uuid.NewString())
			require.Equal(t, intents.StatusPending, row.Status)
			require.Zero(t, fx.gateway.saleCalls.Load())
			store := intents.NewStore(fx.db)
			fx.advanceClock(2 * time.Minute)
			if crash {
				now := fx.runner.Clock.Now()
				_, claimed, err := store.ClaimByID(fx.ctx, row.ID, now, now.Add(time.Minute))
				require.NoError(t, err)
				require.True(t, claimed)
				require.NoError(t, store.MarkUnknown(fx.ctx, row.ID, now, "executor died before send", nil))
				row, err = fx.runner.VerifyByID(fx.ctx, row.ID)
				require.NoError(t, err)
				require.Equal(t, intents.StatusFailedRetryable, row.Status)
				require.Zero(t, fx.gateway.queryCalls.Load(), "unsent verification performs no provider read or write")
				fx.advanceClock(5 * time.Minute)
			}
			// Even a verified unsent retry must re-enter the normal write gate.
			row, err = fx.runner.ExecuteByID(fx.ctx, row.ID)
			require.NoError(t, err)
			require.Equal(t, intents.StatusPending, row.Status)
			require.Zero(t, fx.gateway.saleCalls.Load())
			client.ReadOnly = false
			fx.advanceClock(10 * time.Minute)
			row, err = fx.runner.ExecuteByID(fx.ctx, row.ID)
			require.NoError(t, err)
			require.Equal(t, intents.StatusSucceeded, row.Status)
			require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
			require.Equal(t, 1, fx.paymentCount(t))
		})
	}
}

func TestSaleSubmissionFenceSurvivesStalePark(t *testing.T) {
	fx := newSaleIntentFixture(t)
	store := intents.NewStore(fx.db)
	row, err := store.Enqueue(fx.ctx, intents.EnqueueParams{MerchantID: dbtest.TestMerchantID.UUID(), Provider: "nmi", PspID: fx.payload.Instrument.PSPID, IntentType: payments.TypeNMISale, PriceID: &fx.priceID, Payload: fx.payload, IdempotencyKey: NMISaleIdempotencyKey(uuid.NewString()), Origin: intents.OriginUser})
	require.NoError(t, err)
	now := time.Now().UTC()
	row, claimed, err := store.ClaimByID(fx.ctx, row.ID, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	retained, err := store.RecordProgressIfAbsent(fx.ctx, row.ID, "sale_submitted", true)
	require.NoError(t, err)
	require.True(t, retained)
	require.NoError(t, store.Park(fx.ctx, row.ID, now.Add(time.Minute), "stale read-only observation"))
	current, err := store.Get(fx.ctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status, "a stale park preserves possible submission for read-only recovery")
	require.Nil(t, current.ClaimedUntil)
	require.EqualValues(t, 1, current.Attempts, "a possibly submitted operation never becomes an untouched expirable request")
	client, err := fx.runner.Registry.Lookup(payments.TypeNMISale).(*NMISaleIntentHandler).Sale.ResolveNMIClient(fx.ctx, "mobius")
	require.NoError(t, err)
	client.ReadOnly = true
	outcome := fx.runner.Registry.Lookup(payments.TypeNMISale).Execute(fx.ctx, row)
	require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
	require.NotZero(t, fx.gateway.queryCalls.Load(), "the retained fence routes even a stale executor into read-only verification")
	require.Zero(t, fx.gateway.saleCalls.Load())
}
