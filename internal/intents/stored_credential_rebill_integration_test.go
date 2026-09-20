//go:build integration

package intents

import (
	"context"
	"github.com/stretchr/testify/require"
	"net/url"
	"testing"
)

func (fx rebillFixture) recurringRef(t *testing.T) string {
	t.Helper()
	var ref string
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(), `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, fx.payload.PaymentMethodID).Scan(&ref))
	return ref
}
func (fx rebillFixture) setRecurringRef(t *testing.T, ref string) {
	t.Helper()
	_, err := fx.db.Pool().Exec(context.Background(), `UPDATE billing.payment_methods SET stored_credential_recurring_ref=$2 WHERE id=$1`, fx.payload.PaymentMethodID, ref)
	require.NoError(t, err)
}

func TestManualRebill_AnchoredInstrumentSendsRecurringMIT(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fx.setRecurringRef(t, "anchor-approved-recurring")
	gateway, client := newFakeNMIRebillGateway(t, fx)
	handler := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	accepted, err := handler.EnqueueScheduled(fx.handlerCtx(), fx.subID)
	require.NoError(t, err)
	row, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(fx.handlerCtx(), accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, row.Status)
	form := gateway.saleForm.Load().(url.Values)
	require.Equal(t, "rebill_subscription", form.Get("recurring"))
	require.Equal(t, "merchant", form.Get("initiated_by"))
	require.Equal(t, "used", form.Get("stored_credential_indicator"))
	require.Equal(t, "recurring", form.Get("billing_method"))
	require.Equal(t, "anchor-approved-recurring", form.Get("initial_transaction_id"))
	require.Equal(t, fx.payload.Instrument.RailMethodRef, form.Get("billing_id"))
	require.Equal(t, "anchor-approved-recurring", fx.recurringRef(t))
}

func TestManualRebill_MissingScopedAnchorRefusesAdmission(t *testing.T) {
	for _, unscoped := range []string{"unscoped-initial-transaction", ""} {
		t.Run(unscoped, func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			fx.setRecurringRef(t, "")
			_, err := fx.db.Pool().Exec(context.Background(), `UPDATE billing.payment_methods SET initial_transaction_id=$2 WHERE id=$1`, fx.payload.PaymentMethodID, unscoped)
			require.NoError(t, err)
			gateway, client := newFakeNMIRebillGateway(t, fx)
			handler := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
			_, err = handler.EnqueueScheduled(fx.handlerCtx(), fx.subID)
			require.Error(t, err)
			require.Zero(t, gateway.saleCalls.Load())
			var operations int
			require.NoError(t, fx.db.Pool().QueryRow(context.Background(), `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1`, fx.subID).Scan(&operations))
			require.Zero(t, operations, "an incomplete agreement is not accepted and later refreshed")
		})
	}
}

func TestManualRebill_ChangedAnchorRefusesBeforeSubmission(t *testing.T) {
	fx := seedPastDueSubscription(t)
	gateway, client := newFakeNMIRebillGateway(t, fx)
	handler := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	accepted, err := handler.EnqueueScheduled(fx.handlerCtx(), fx.subID)
	require.NoError(t, err)
	fx.setRecurringRef(t, "another-approved-agreement")
	row, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(fx.handlerCtx(), accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusFailedTerminal, row.Status)
	require.Zero(t, gateway.saleCalls.Load())
	require.EqualValues(t, 1, *fx.subscription(t).RetryAttempts, "pre-send supersession never consumes a financial decline")
	next, err := handler.EnqueueScheduled(fx.handlerCtx(), fx.subID)
	require.NoError(t, err)
	require.NotEqual(t, accepted.ID, next.ID)
	current, err := DecodeManualRebillPayload(next)
	require.NoError(t, err)
	require.Equal(t, 1, current.Attempt)
	require.Equal(t, 1, current.FailureCount)
}
