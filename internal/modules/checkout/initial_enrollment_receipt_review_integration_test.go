//go:build integration

package checkout

import (
	"testing"

	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A native paid-now enrollment needs a qualified initial charge. An approved
// schedule identifier alone cannot buy standing access or complete the intent.
func TestInitialEnrollmentMissingChargeCannotActivatePaidAccess(t *testing.T) {
	fx := newSubIntentFixture(t)
	require.Positive(t, fx.payload.AmountMicros)
	require.Empty(t, fx.payload.StartDate)
	require.Nil(t, fx.payload.DelayedStart)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"initial_paid_access":null}' WHERE id=(SELECT product_id FROM billing.prices WHERE id=$1)`, fx.priceID)
	require.NoError(t, err)
	fx.gateway.txnID = ""
	result := fx.enqueueAndExecute(t)
	var active, payments, access int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE id=$1 AND status='active'`, fx.payload.LocalSubscriptionID).Scan(&active))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, fx.payload.LocalSubscriptionID).Scan(&payments))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.entitlements WHERE source_type='subscription' AND source_id=$1 AND entitlement='initial_paid_access' AND revoked_at IS NULL AND deleted_at IS NULL`, fx.payload.LocalSubscriptionID).Scan(&access))
	t.Logf("one approved enrollment without a charge identifier: intent=%s active_subscriptions=%d payments=%d access_windows=%d", result.Status, active, payments, access)
	assert.Equal(t, intents.StatusUnknownNeedsVerify, result.Status)
	assert.Zero(t, active, "a paid-now subscription cannot activate from schedule-only evidence")
	assert.Zero(t, access, "no paid entitlement may be granted without its exact initial payment")
	require.Zero(t, payments, "missing provider money must not be fabricated")
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
}
