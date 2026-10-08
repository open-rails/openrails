//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// NMI does not promise bounded Query API visibility or duplicate checking on
// every processor. An empty lookup cannot release an uncertain submission.
func TestEngineNMIHiddenChargeNeverResubmitted(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	end := e.periodEnd()
	e.toPeriodEnd()
	w.nmi.UnsupportedDuplicateChecking(true)
	w.nmi.HideSales(1)
	w.nmi.DropSaleResponses(1)
	w.runRenewals()
	require.Len(t, e.providerLedger(), 2, "the provider committed the renewal but lost its response")
	for range 6 {
		w.advance(time.Hour)
		w.wake()
	}
	require.Len(t, e.providerLedger(), 2, "empty order and vault reads must never authorize another sale")
	require.Equal(t, 2, e.providerAttempts(), "only initial enrollment and one renewal submission")
	require.True(t, e.periodEnd().Equal(end), "the unknown payment remains pending")
	require.Len(t, w.openFindings("life.submission.unresolved"), 1)
	w.nmi.Reveal()
	w.until(func() bool { return e.periodEnd().After(end) }, "the visible provider receipt resumes local completion")
	require.Len(t, e.providerLedger(), 2)
	require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
	require.Empty(t, w.openFindings("life.submission.unresolved"))
}
