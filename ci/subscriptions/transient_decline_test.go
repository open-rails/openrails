//go:build greenfield && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A processor try-again answer is retried within minutes on a short bounded
// ladder that does not count as a dunning failure; after the ladder it is an
// ordinary decline on the cycle's schedule.
func TestEngineTransientDeclineLadder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.setDecline(visa.Last4, "processing_error", "")
	e.toPeriodEnd()
	for i, gap := range []time.Duration{5 * time.Minute, 30 * time.Minute} {
		at := w.clock.Now()
		w.runRenewals()
		sub := w.subscription(embedded, e.sub)
		require.Equal(t, "past_due", sub.Status, "transient retry %d", i+1)
		require.NotNil(t, sub.NextRetryAt)
		require.Equal(t, gap, sub.NextRetryAt.Sub(at).Round(time.Minute), "transient retry %d", i+1)
		w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
	}
	first := w.clock.Now()
	w.runRenewals()
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, "past_due", sub.Status)
	require.Equal(t, 48*time.Hour, sub.NextRetryAt.Sub(first).Round(time.Hour), "after the ladder the monthly schedule applies")
	require.Equal(t, 3, e.providerAttempts()-1, "three renewal attempts so far")

	e.setDecline(visa.Last4, "", "")
	w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
	w.runRenewals()
	require.Equal(t, "active", w.subscription(embedded, e.sub).Status)
	require.Len(t, e.providerLedger(), 2, "one renewal charge")
}
