//go:build integration

package checkout

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
)

func operatorResolutionLogs(t *testing.T, database *db.DB, intentID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, database.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM openrails.rail_mutation_logs WHERE rail_intent_id=$1 AND evidence ? 'operator_resolution'`, intentID).Scan(&n))
	return n
}

// A sale that landed but stays invisible to the order search remains unknown
// across restart until an exact transaction, matched on vault and amount,
// resolves it; local purchase effects then commit once and nothing re-sends.
func TestNMISaleUnknownResolvesFromExactReceipt(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("timeout-after-accept")
	fx.gateway.hidden.Store(true)
	key := "sale-resolve-" + uuid.NewString()[:8]
	intent := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status)

	restart := *fx.runner
	restart.Store = intents.NewStore(fx.db)
	fx.runner = &restart
	fx.advanceClock(2 * time.Minute)
	_, err := fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)

	fx.gateway.vault.Store("vault-other")
	_, err = fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{ProviderReference: fx.gateway.txnID, Actor: "ops", Reason: "dashboard"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected, "a transaction on another vault is not this sale")
	_, err = fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{ProviderReference: "txn-missing", Actor: "ops", Reason: "dashboard"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.Equal(t, 0, fx.paymentCount(t))

	fx.gateway.vault.Store(fx.payload.CustomerVaultID)
	resolved, err := fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{ProviderReference: fx.gateway.txnID, Actor: "ops@example.test", Reason: "NMI ticket 7"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, resolved.Status)
	require.Equal(t, 1, fx.paymentCount(t))
	require.Equal(t, 1, operatorResolutionLogs(t, fx.db, intent.ID))

	replay := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusSucceeded, replay.Status)
	_, err = fx.runner.RunVerifyOnce(fx.ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load(), "resolution and replay never re-send")
	require.Equal(t, 1, fx.paymentCount(t))
}

// Non-execution is refused while the exact order search shows a successful
// sale, and otherwise terminates the operation without any local charge.
func TestNMISaleNonExecutionRequiresUncontradictedOrder(t *testing.T) {
	fx := newSaleIntentFixture(t)
	fx.gateway.saleMode.Store("ambiguous500")
	key := "sale-absent-" + uuid.NewString()[:8]
	intent := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusUnknownNeedsVerify, intent.Status)

	_, err := fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "wrong"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.Equal(t, 0, fx.paymentCount(t))

	fx.gateway.charged.Store(false) // provider confirms the request never landed
	resolved, err := fx.runner.Resolve(fx.ctx, intent.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "NMI ticket 8"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedTerminal, resolved.Status)
	replay := fx.enqueueAndExecute(t, key)
	require.Equal(t, intents.StatusFailedTerminal, replay.Status)
	require.EqualValues(t, 1, fx.gateway.saleCalls.Load())
	require.Equal(t, 0, fx.paymentCount(t))
}
