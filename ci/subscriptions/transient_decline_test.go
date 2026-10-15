//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// A processor try-again answer is retried within minutes on a short bounded
// ladder that does not count as a dunning failure; after the ladder it is an
// ordinary decline on the cycle's schedule.
func TestEngineTransientDeclineLadder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.refreshBeforePeriodEnd()
	e.setDecline(visa.Last4, "processing_error", "")
	e.toPeriodEnd()
	for i, gap := range []time.Duration{5 * time.Minute, 30 * time.Minute} {
		at := w.clock.Now()
		w.runRenewals()
		sub := w.subscription(embedded, e.sub)
		require.Equal(t, billing.SubscriptionPastDue, sub.Status, "transient retry %d", i+1)
		require.NotNil(t, nextRetry(sub))
		require.Equal(t, gap, nextRetry(sub).Sub(at).Round(time.Minute), "transient retry %d", i+1)
		var failures []map[string]any
		for _, raw := range e.c.notificationEvents() {
			event := raw.(map[string]any)
			if event["event_type"] == "payment_method_failed" {
				failures = append(failures, event["data"].(map[string]any))
			}
		}
		require.Len(t, failures, i+1, "each real declined attempt produces its retry guidance")
		latest := failures[len(failures)-1]
		require.Equal(t, "scheduled", latest["retry_status"])
		require.Equal(t, "update_payment_method", latest["recovery_action"])
		require.Equal(t, nextRetry(sub).UTC().Format(time.RFC3339Nano), latest["next_attempt_at"])
		require.Equal(t, e.sub.String(), latest["subscription_id"])
		w.runRenewals()
		require.Equal(t, i+1, e.c.notificationCount("payment_method_failed"), "replaying the completed attempt adds no failure notice")
		w.advanceHealthyTo(nextRetry(sub).Add(time.Second))
	}
	first := w.clock.Now()
	w.runRenewals()
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	require.Equal(t, 48*time.Hour, nextRetry(sub).Sub(first).Round(time.Hour), "after the ladder the monthly schedule applies")
	require.Equal(t, 3, e.providerAttempts()-1, "three renewal attempts so far")

	e.setDecline(visa.Last4, "", "")
	w.advanceHealthyTo(nextRetry(sub).Add(time.Second))
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
	require.Len(t, e.providerLedger(), 2, "one renewal charge")
}
