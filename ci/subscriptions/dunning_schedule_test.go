//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// The same soft decline on an engine and an NMI-owned renewal, the NMI one
// seen 30 hours late through the Query API: both retry on the same days,
// counted from the decline (#1113).
func TestDunningScheduleParity(t *testing.T) {
	t.Parallel()
	want := []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}
	for _, owner := range []string{"engine", "nmi_schedule"} {
		t.Run(owner, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			var sub billing.SubscriptionID
			if owner == "engine" {
				e := enroll(t, w, "nmi", embedded)
				e.refreshBeforePeriodEnd()
				e.setDecline(visa.Last4, "insufficient_funds", "202")
				e.toPeriodEnd()
				w.runRenewals()
				sub = e.sub
			} else {
				l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
				w.converge()
				w.nmi.SetDecline(visa.Last4, "202")
				due := l.periodEnd()
				w.nmi.RenewSchedule(l.railSub, false)
				w.advance(due.Sub(w.clock.Now()) + 30*time.Hour)
				w.watchRebills()
				w.refreshProviders()
				sub = l.sub
			}
			rows := w.cycleAttempts(sub)
			require.Len(t, rows, 1)
			declined := rows[0].AttemptedAt
			var got []time.Duration
			for i := range want {
				s := w.subscription(embedded, sub)
				require.Equal(t, billing.SubscriptionPastDue, s.Status)
				require.NotNil(t, nextRetry(s))
				got = append(got, nextRetry(s).Sub(declined))
				if i < len(want)-1 {
					w.advanceHealthyTo(*nextRetry(s))
					w.runRenewals()
				}
			}
			require.Equal(t, want, got)
		})
	}
}
