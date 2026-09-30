//go:build greenfield && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
)

type rebillWatchPass struct{}

func (rebillWatchPass) Kind() string { return "openrails.rebill_watch" }

// watchRebills runs the missed-rebill watch once (#1112).
func (w *world) watchRebills() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), rebillWatchPass{}, &river.InsertOpts{Queue: embed.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
	w.settle()
}

// missReason is the recorded miss of the subscription's cycle due at due, or "".
func (w *world) missReason(sub openrails.SubscriptionID, due time.Time) string {
	w.t.Helper()
	var reason *string
	err := w.pool.QueryRow(w.t.Context(), `SELECT miss_reason FROM `+pgx.Identifier{w.schema}.Sanitize()+`.rebill_cycles
		WHERE subscription_id = $1 AND due_at = $2`, sub.UUID(), due).Scan(&reason)
	if err == pgx.ErrNoRows {
		return ""
	}
	require.NoError(w.t, err)
	return str(reason)
}

// NMI passes a schedule's date without charging: after the deadline the
// Query API proves no attempt, and the cycle is a missed rebill.
func TestNMIScheduleSkippedRebillIsMissed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
	w.converge()
	due := l.periodEnd()
	w.nmi.SkipSchedule(l.railSub)
	w.advance(due.Sub(w.clock.Now()) + 12*time.Hour)
	w.watchRebills()
	require.Empty(t, w.missReason(l.sub, due), "inside NMI's deadline nothing is decided")
	w.advance(13 * time.Hour)
	w.watchRebills()
	require.Equal(t, "provider_skipped", w.missReason(l.sub, due))
	require.Len(t, w.openFindings("life.rebill.missed"), 1)
	require.Zero(t, len(w.nmi.Attempts()), "a missed NMI rebill is not charged here")
	w.watchRebills()
	require.Len(t, w.openFindings("life.rebill.missed"), 1, "one finding per cycle")
}

// NMI charged and its webhook never came: the watch finds the charge through
// the Query API, records it as observed by pull, and renews the period.
func TestNMIScheduleLostWebhookFoundByPull(t *testing.T) {
	t.Parallel()
	for _, paid := range []bool{true, false} {
		t.Run(map[bool]string{true: "approved", false: "declined"}[paid], func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
			w.converge()
			if !paid {
				w.nmi.SetDecline(visa.Last4, "202")
			}
			due := l.periodEnd()
			w.nmi.RenewSchedule(l.railSub, paid)
			w.advance(due.Sub(w.clock.Now()) + 25*time.Hour)
			w.watchRebills()
			rows := w.cycleAttempts(l.sub)
			require.Len(t, rows, 1)
			require.Equal(t, []string{"rebill", "provider_schedule", "pull"}, []string{rows[0].Kind, rows[0].Source, rows[0].ObservedVia})
			require.Empty(t, w.missReason(l.sub, due))
			sub := w.subscription(embedded, l.sub)
			if paid {
				require.Equal(t, "approved", rows[0].Category)
				require.Equal(t, "active", sub.Status)
				require.True(t, sub.CurrentPeriodEndsAt.After(due), "the period is renewed")
				return
			}
			require.Equal(t, "insufficient_funds", str(rows[0].Reason))
			require.Equal(t, "past_due", sub.Status, "the decline opens dunning")
			require.NotNil(t, sub.NextRetryAt)
		})
	}
}

// An engine renewal held by the operator is a missed rebill past its hour.
func TestEngineHeldRebillIsMissed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	due := e.periodEnd()
	w.cfg = func(c *config.Config) { c.EngineAdmissionHold = true }
	w.restart()
	w.advance(due.Sub(w.clock.Now()) + 2*time.Hour)
	w.runRenewals()
	w.watchRebills()
	require.Equal(t, "held", w.missReason(e.sub, due))
	require.Equal(t, 1, e.providerAttempts(), "only the initial charge")
}
