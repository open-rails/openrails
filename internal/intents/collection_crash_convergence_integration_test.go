//go:build integration

package intents

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #828 subscription-consumer crash-window convergence: the executor dies with
// the charge outcome lost, a rerun of the SAME dunning decision re-derives
// the SAME idempotency key and gets the durable in-doubt intent back instead
// of charging again, and the verifier's provider read converges the period —
// exactly one sale ever reaches the gateway. The invoice-consumer twin lives
// in internal/modules/money/invoice_verify_integration_test.go.
func TestManualRebillCrashRerunConvergesWithoutDoubleCharge(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)
	fake.saleStatus.Store(http.StatusBadGateway) // outcome lost mid-flight (crash window)

	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	require.EqualValues(t, 1, fake.saleCalls.Load())

	// The crashed worker's replacement re-runs the same dunning decision while
	// the gateway is healthy again: the idempotency-key conflict returns the
	// durable in-doubt row UNTOUCHED — no blind re-charge.
	fake.saleStatus.Store(0)
	rerun, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, row.ID, rerun.ID)
	assert.Equal(t, StatusUnknownNeedsVerify, rerun.Status, "an in-doubt attempt must verify, never re-execute")
	assert.EqualValues(t, 1, fake.saleCalls.Load(), "rerun sent no second charge")
	assert.Equal(t, "past_due", string(fx.subscription(t).Status))

	// The charge actually landed; the verifier's provider read converges the
	// period and repairs the lifecycle.
	fake.charged.Store(true)
	_, err = fx.db.Pool().Exec(context.Background(),
		"UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE id = $1", row.ID)
	require.NoError(t, err)
	_, err = fx.rebillRunner(client, fullModeConfig()).RunVerifyOnce(context.Background())
	require.NoError(t, err)

	assert.Equal(t, StatusSucceeded, fx.intentByID(t, row.ID).Status)
	sub := fx.subscription(t)
	assert.Equal(t, "active", string(sub.Status), "verifier converged the crashed charge into the renewal")
	require.NotNil(t, sub.CurrentPeriodEndsAt)
	assert.True(t, sub.CurrentPeriodEndsAt.After(fx.periodEnd))
	assert.EqualValues(t, 1, fake.saleCalls.Load(), "exactly one charge ever reached the gateway")
}
