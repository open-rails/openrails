//go:build integration

package intents

import (
	"context"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #297 dunning proof: every manual rebill is a merchant-initiated RECURRING
// credential-on-file charge. An anchored instrument replays its recurring
// reference; legacy rows fall back to their original vault transaction; and a
// fully unanchored historical row still attempts an observable best-effort MIT.

func (fx rebillFixture) recurringRef(t *testing.T) string {
	t.Helper()
	var ref string
	require.NoError(t, fx.db.Pool().QueryRow(context.Background(),
		`SELECT pm.stored_credential_recurring_ref
		 FROM openrails.payment_methods pm
		 JOIN openrails.subscriptions s ON s.payment_method_id = pm.id
		 WHERE s.id = $1`, fx.subID).Scan(&ref))
	return ref
}

func (fx rebillFixture) setRecurringRef(t *testing.T, ref string) {
	t.Helper()
	_, err := fx.db.Pool().Exec(context.Background(),
		`UPDATE openrails.payment_methods pm SET stored_credential_recurring_ref = $2
		 FROM openrails.subscriptions s
		 WHERE s.payment_method_id = pm.id AND s.id = $1`, fx.subID, ref)
	require.NoError(t, err)
}

func (fx rebillFixture) setLegacyInitialRef(t *testing.T, ref string) {
	t.Helper()
	_, err := fx.db.Pool().Exec(context.Background(),
		`UPDATE openrails.payment_methods pm SET initial_transaction_id = $2
		 FROM openrails.subscriptions s
		 WHERE s.payment_method_id = pm.id AND s.id = $1`, fx.subID, ref)
	require.NoError(t, err)
}

func TestManualRebill_AnchoredInstrumentSendsRecurringMIT(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fx.setRecurringRef(t, "anchor-297-rec")
	fake, client := newFakeNMIRebillGateway(t)

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, row.Status)

	form := fake.saleForm.Load().(url.Values)
	assert.Equal(t, "rebill_subscription", form.Get("recurring"))
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, "recurring", form.Get("billing_method"))
	assert.Equal(t, "anchor-297-rec", form.Get("initial_transaction_id"))

	assert.Equal(t, "anchor-297-rec", fx.recurringRef(t), "existing anchor is never overwritten")
}

func TestManualRebill_UsesLegacyInitialTransactionWhenScopedAnchorIsMissing(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fx.setRecurringRef(t, "")
	fake, client := newFakeNMIRebillGateway(t)

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, row.Status)
	assert.Equal(t, int64(1), fake.saleCalls.Load())
	form := fake.saleForm.Load().(url.Values)
	assert.Contains(t, form.Get("initial_transaction_id"), "txn-init-", "legacy vault-creation transaction is the next-best recoverable anchor")
	assert.Empty(t, fx.recurringRef(t), "legacy fallback is not promoted into the agreement-scoped field")
}

func TestManualRebill_UnrecoverableAnchorStillAttemptsBestEffortMIT(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fx.setRecurringRef(t, "")
	fx.setLegacyInitialRef(t, "")
	fake, client := newFakeNMIRebillGateway(t)

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, row.Status)
	assert.Equal(t, int64(1), fake.saleCalls.Load())
	form := fake.saleForm.Load().(url.Values)
	assert.Equal(t, "merchant", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, "recurring", form.Get("billing_method"))
	assert.NotContains(t, form, "initial_transaction_id", "only the unrecoverable anchor is omitted")
	assert.Empty(t, fx.recurringRef(t), "a fallback MIT must never masquerade as the original CIT anchor")
}
