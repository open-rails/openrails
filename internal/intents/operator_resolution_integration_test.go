//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

func operatorLogCount(t *testing.T, fx refundFixture, id uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM openrails.rail_mutation_logs WHERE rail_intent_id=$1 AND evidence ? 'operator_resolution'`, id).Scan(&n))
	return n
}

// A processor communication response does not prove the refund did not land:
// the reservation stays open and the verifier never releases it.
func TestNMIRefundUncertainResponseCodeKeepsReservation(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
	fake.refundBody.Store(`{"object":"transaction","id":"txn_refund_1","response":"3","response_text":"PROCESSOR COMMUNICATION ERROR","response_code":"421"}`)
	runner := fx.refundRunner(client, fullModeConfig())
	row, err := runner.EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	_, err = fx.db.Pool().Exec(context.Background(), "UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1", row.ID)
	require.NoError(t, err)
	_, err = runner.RunVerifyOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentByID(t, row.ID).Status)
	status, _, _ := fx.reservation(t)
	require.Equal(t, "pending", status)
	require.EqualValues(t, 1, fake.refundCalls.Load())
}

// A lost refund response converges only from the exact provider refund, read
// back and matched to the reservation, and never re-sends.
func TestNMIRefundUnknownResolvesOnlyFromExactReceipt(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
	fake.refundStatus.Store(http.StatusBadGateway)
	runner := fx.refundRunner(client, fullModeConfig())
	row, err := runner.EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	fake.refunded.Store(true)
	ctx := dbtest.WithTestMerchant(context.Background())

	_, err = runner.Resolve(ctx, row.ID, Resolution{ProviderReference: "txn_refund_1", Reason: "provider dashboard"})
	require.ErrorIs(t, err, ErrResolutionInvalid)
	_, err = runner.Resolve(ctx, row.ID, Resolution{ProviderReference: "txn_refund_1", NotExecuted: true, Actor: "ops", Reason: "both"})
	require.ErrorIs(t, err, ErrResolutionInvalid)
	_, err = runner.Resolve(ctx, row.ID, Resolution{ProviderReference: "txn_other", Actor: "ops", Reason: "wrong id"})
	require.ErrorIs(t, err, ErrResolutionRejected)
	got := fx.intentByID(t, row.ID)
	require.Equal(t, StatusUnknownNeedsVerify, got.Status)
	require.Nil(t, got.ClaimedUntil, "rejected evidence releases the lease")
	status, _, _ := fx.reservation(t)
	require.Equal(t, "pending", status)

	resolved, err := (&Runner{Store: NewStore(fx.db), Registry: NewRegistry(NewNMIRefundHandler(fx.db, fakeNMIResolver{client: client}, nil)), Config: fullModeConfig()}).
		Resolve(ctx, row.ID, Resolution{ProviderReference: "txn_refund_1", Actor: "ops@example.test", Reason: "NMI ticket 42"})
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, resolved.Status)
	status, txn, _ := fx.reservation(t)
	require.Equal(t, "completed", status)
	require.Equal(t, "txn_refund_1", txn)
	require.EqualValues(t, 1, fake.refundCalls.Load(), "resolution never re-sends")
	require.Equal(t, 1, operatorLogCount(t, fx, row.ID))

	_, err = runner.Resolve(ctx, row.ID, Resolution{ProviderReference: "txn_refund_1", Actor: "ops", Reason: "again"})
	require.ErrorIs(t, err, ErrResolutionNotUnknown)
	_, err = runner.RunExecuteOnce(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, fake.refundCalls.Load())
}

// Neither a missing refund in the provider read nor another matching refund
// proves that this possibly submitted operation never ran.
func TestNMIRefundUnknownNonExecutionPreservesReservation(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(fmt.Sprint(visible), func(t *testing.T) {
			fx := seedRefundablePayment(t, 500)
			fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
			fake.refundStatus.Store(http.StatusBadGateway)
			runner := fx.refundRunner(client, fullModeConfig())
			row, err := runner.EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
			require.NoError(t, err)
			fake.refunded.Store(visible)
			_, err = runner.Resolve(dbtest.WithTestMerchant(context.Background()), row.ID, Resolution{NotExecuted: true, Actor: "ops", Reason: "provider search"})
			require.ErrorIs(t, err, ErrResolutionRejected)
			require.Equal(t, StatusUnknownNeedsVerify, fx.intentByID(t, row.ID).Status)
			status, _, _ := fx.reservation(t)
			require.Equal(t, "pending", status)
			_, err = runner.RunExecuteOnce(context.Background())
			require.NoError(t, err)
			require.EqualValues(t, 1, fake.refundCalls.Load(), "nonexecution cannot release funds for another refund")
		})
	}
}

// A successful refund whose local receipt write fails keeps the exact provider
// id on the operation; after restart the verifier completes it without resend.
func TestNMIRefundReceiptSurvivesLocalFailureAcrossRestart(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	fake, client := newFakeNMIRefundGateway(t, fx.originalTxn)
	ddl := dbtest.SharedSuperuserPGXPool(t)
	constraint := "test_refund_receipt_" + fx.reservationID.String()[:8]
	_, err := ddl.Exec(context.Background(), fmt.Sprintf(`ALTER TABLE openrails.payments ADD CONSTRAINT %s CHECK (id <> '%s'::uuid OR NOT (coalesce(metadata,'{}') ? 'provider_refund_id'))`, constraint, fx.reservationID))
	require.NoError(t, err)
	remove := func() {
		_, err := ddl.Exec(context.Background(), `ALTER TABLE openrails.payments DROP CONSTRAINT IF EXISTS `+constraint)
		require.NoError(t, err)
	}
	t.Cleanup(remove)
	row, err := fx.refundRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(500))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	require.Equal(t, "txn_refund_1", EvidenceString(fx.intentByID(t, row.ID), "provider_refund_id"))
	status, _, _ := fx.reservation(t)
	require.Equal(t, "pending", status)
	remove()

	_, err = fx.db.Pool().Exec(context.Background(), "UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1", row.ID)
	require.NoError(t, err)
	_, err = fx.refundRunner(client, fullModeConfig()).RunVerifyOnce(dbtest.WithTestMerchant(context.Background()))
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
	status, txn, _ := fx.reservation(t)
	require.Equal(t, "completed", status)
	require.Equal(t, "txn_refund_1", txn)
	require.EqualValues(t, 1, fake.refundCalls.Load())
}

// A Stripe refund whose response and list visibility are both lost converges
// by replaying the same provider idempotency key; the provider returns the
// original refund and exactly one refund exists.
func TestStripeRefundLostResponseReplaysProviderIdempotencyKey(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	stripe := newFakeStripeServer(t)
	stripe.createStatus.Store(http.StatusInternalServerError)
	stripe.dedupe.Store(true)
	cfg := stripeIntegrationConfig(config.ProviderWriteModeFull)
	row, err := fx.stripeRunner(cfg, stripe.srv.URL).EnqueueAndExecute(context.Background(), fx.stripeEnqueueParams(t, 500))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	require.EqualValues(t, 1, stripe.refundObjects.Load(), "the lost create landed")

	stripe.createStatus.Store(0)
	stripe.listHidden.Store(true)
	_, err = fx.db.Pool().Exec(context.Background(), "UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1", row.ID)
	require.NoError(t, err)
	_, err = fx.stripeRunner(cfg, stripe.srv.URL).RunVerifyOnce(context.Background())
	require.NoError(t, err)
	require.Equal(t, StatusFailedRetryable, fx.intentByID(t, row.ID).Status)
	_, err = fx.db.Pool().Exec(context.Background(), "UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1", row.ID)
	require.NoError(t, err)
	_, err = fx.stripeRunner(cfg, stripe.srv.URL).RunExecuteOnce(dbtest.WithTestMerchant(context.Background()))
	require.NoError(t, err)

	require.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
	require.EqualValues(t, 2, stripe.creates.Load(), "the replay reuses the provider-enforced key")
	require.EqualValues(t, 1, stripe.refundObjects.Load(), "provider idempotency returns the original refund")
	status, txn, _ := fx.reservation(t)
	require.Equal(t, "completed", status)
	require.Equal(t, "re_1", txn)
}

// A lost rebill resolves only through the exact order-reference search or
// provider-confirmed non-execution; an operator reference is refused, and
// non-execution is refused while the order shows a successful sale.
func TestManualRebillOperatorResolutionRequiresQualifiedReceipt(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t, fx)
	fake.saleStatus.Store(http.StatusBadGateway)
	runner := fx.rebillRunner(client, fullModeConfig())
	row, err := runner.EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	ctx := dbtest.WithTestMerchant(context.Background())

	_, err = runner.Resolve(ctx, row.ID, Resolution{ProviderReference: fake.txnID, Actor: "ops", Reason: "dashboard"})
	require.ErrorIs(t, err, ErrResolutionRejected)
	fake.charged.Store(true)
	_, err = runner.Resolve(ctx, row.ID, Resolution{NotExecuted: true, Actor: "ops", Reason: "wrong"})
	require.ErrorIs(t, err, ErrResolutionRejected)
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentByID(t, row.ID).Status)

	fake.charged.Store(false)
	_, err = runner.Resolve(ctx, row.ID, Resolution{NotExecuted: true, Actor: "ops", Reason: "empty search"})
	require.ErrorIs(t, err, ErrResolutionRejected)
	require.Equal(t, StatusUnknownNeedsVerify, fx.intentByID(t, row.ID).Status)
	fake.charged.Store(true)
	resolved, err := runner.Resolve(ctx, row.ID, Resolution{ProviderReference: fake.txnID, Actor: "ops", Reason: "exact receipt"})
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, resolved.Status)
	require.Equal(t, "active", string(fx.subscription(t).Status))
	require.EqualValues(t, 1, fake.saleCalls.Load())

}
