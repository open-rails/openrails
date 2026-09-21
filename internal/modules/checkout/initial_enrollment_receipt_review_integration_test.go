//go:build integration

package checkout

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A native paid-now enrollment needs a qualified initial charge. An approved
// schedule identifier alone cannot buy standing access or complete the intent.
func initialEnrollmentMissingChargeCannotActivatePaidAccess(t *testing.T) {
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

// Run this bounded control alone: ProxyFromEnvironment caches its environment.
// Refuse to boot a fixture unless every external provider URL is guarded.
func TestInitialEnrollmentAcceptedModes(t *testing.T) {
	var blocked atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blocked.Add(1)
		t.Logf("blocked unexpected HTTP egress to %s", r.Host)
		http.Error(w, "local enrollment control forbids external HTTP", http.StatusBadGateway)
	}))
	t.Cleanup(guard.Close)
	t.Setenv("HTTPS_PROXY", guard.URL)
	t.Setenv("HTTP_PROXY", guard.URL)
	t.Setenv("NO_PROXY", "127.0.0.1,localhost,::1")
	proxy, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: "secure.nmi.com"}})
	require.NoError(t, err)
	require.NotNil(t, proxy)
	require.Equal(t, guard.URL, proxy.String(), "run this test alone before HTTP proxy configuration is cached")
	t.Cleanup(func() { require.Zero(t, blocked.Load(), "unexpected external HTTP attempt was blocked") })
	t.Run("paid_now_requires_charge", initialEnrollmentMissingChargeCannotActivatePaidAccess)
	for _, mode := range []string{"covered_delay", "free_recurring"} {
		t.Run(mode, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			if mode == "covered_delay" {
				start := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour)
				fx.payload.StartDate, fx.payload.DelayedStart = start.Format("20060102"), &start
			} else {
				fx.payload.AmountMicros = 0
				_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET amount=0 WHERE id=$1`, fx.priceID)
				require.NoError(t, err)
			}
			fx.gateway.txnID = ""
			fx.enqueueAndExecute(t)
			form, ok := fx.gateway.createForm.Load().(url.Values)
			require.True(t, ok)
			assert.Equal(t, "add_subscription", form.Get("recurring"))
			assert.NotContains(t, form, "type", "a no-charge enrollment must not dispatch a sale")
			assert.NotContains(t, form, "amount", "recurring schedule amount is not an initial charge")
			assert.NotContains(t, form, "stored_credential_indicator", "no transaction established a recurring credential anchor")
			assert.Equal(t, fx.payload.StartDate, form.Get("start_date"))
			var payments int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, fx.payload.LocalSubscriptionID).Scan(&payments))
			require.Zero(t, payments)
		})
	}
}
