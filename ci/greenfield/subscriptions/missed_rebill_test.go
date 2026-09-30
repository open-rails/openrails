//go:build greenfield && integration

package subscriptions_test

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/nmimock"
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
// Query API proves no attempt, the cycle is a missed rebill, and OpenRails
// charges it once. NMI's next charge bills only the next period (#1113).
func TestNMIScheduleSkippedRebillIsCollected(t *testing.T) {
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
	w.watchRebills()
	require.Len(t, w.openFindings("life.rebill.missed"), 1, "one finding per cycle")
	require.Equal(t, "past_due", w.subscription(embedded, l.sub).Status, "the period is OpenRails' to collect")

	w.runRenewals()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, "active", sub.Status)
	next := *sub.CurrentPeriodEndsAt
	require.True(t, next.After(due))
	require.Len(t, w.nmi.Attempts(), 1, "OpenRails charges the skipped period once")
	rows := w.cycleAttempts(l.sub)
	require.Len(t, rows, 1)
	require.Equal(t, []string{"rebill", "nmi_schedule", "openrails", "approved"}, []string{rows[0].Kind, rows[0].Owner, rows[0].Source, rows[0].Category})
	require.True(t, due.Equal(rows[0].DueAt))

	w.advance(next.Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
	w.settle()
	w.runRenewals()
	require.True(t, w.subscription(embedded, l.sub).CurrentPeriodEndsAt.After(next))
	require.Len(t, w.nmi.Attempts(), 1)
	require.Len(t, w.nmi.ledger(""), 3, "one charge per period: the initial, OpenRails' collection, NMI's next")
	require.Len(t, completed(w.payments(embedded, l.c.id)), 3)
}

// The guards between a skipped NMI period and OpenRails' charge (#1113):
// without proof from NMI's records, with provider writes off, or when NMI's
// schedule no longer shows the next period at charge time, nothing is charged.
func TestNMISkippedRebillGuards(t *testing.T) {
	t.Parallel()
	skipped := func(t *testing.T, w *world, skip func(*legacy)) (*legacy, time.Time) {
		l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
		w.converge()
		due := l.periodEnd()
		skip(l)
		w.advance(due.Sub(w.clock.Now()) + 25*time.Hour)
		return l, due
	}
	skip := func(l *legacy) { l.w.nmi.SkipSchedule(l.railSub) }
	t.Run("query_api_down", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l, due := skipped(t, w, skip)
		queryAPI := func(r *http.Request) bool {
			form, err := url.ParseQuery(readBody(r))
			return strings.HasSuffix(r.URL.Path, "/query.php") || (err == nil && form.Has("report_type"))
		}
		w.nmi.Intercept(queryAPI,
			func(*http.Request, func() *http.Response) (*http.Response, error) {
				return nil, errors.New("query api unreachable")
			})
		w.watchRebills()
		w.runRenewals()
		require.Empty(t, w.missReason(l.sub, due), "no proof, no miss")
		require.Equal(t, "active", w.subscription(embedded, l.sub).Status)
		require.Zero(t, len(w.nmi.Attempts()))
		w.nmi.ClearIntercepts()
		w.watchRebills()
		require.Equal(t, "provider_skipped", w.missReason(l.sub, due), "the next pass decides")
	})
	t.Run("schedule_gone", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l, due := skipped(t, w, func(l *legacy) { l.w.nmi.DeleteSchedule(l.railSub) })
		// NMI answers 404 for a deleted schedule.
		w.nmi.Intercept(func(r *http.Request) bool {
			return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/subscriptions/"+l.railSub)
		}, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
			res := serve()
			res.StatusCode, res.Body = http.StatusNotFound, io.NopCloser(strings.NewReader(`{"type":"notFound","error_code":"E_NOT_FOUND"}`))
			return res, nil
		})
		w.watchRebills()
		w.runRenewals()
		require.Equal(t, "schedule_gone", w.missReason(l.sub, due))
		require.NotEqual(t, "past_due", w.subscription(embedded, l.sub).Status)
		require.Zero(t, len(w.nmi.Attempts()), "a deleted schedule only raises the finding")
	})
	t.Run("read_only", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l, due := skipped(t, w, skip)
		w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly }
		w.restart()
		w.watchRebills()
		w.runRenewals()
		require.Equal(t, "provider_skipped", w.missReason(l.sub, due))
		require.Equal(t, "active", w.subscription(embedded, l.sub).Status, "read-only records the miss and collects nothing")
		require.Zero(t, len(w.nmi.Attempts()))
	})
	t.Run("schedule_moved_back", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l, due := skipped(t, w, skip)
		w.watchRebills()
		require.Equal(t, "past_due", w.subscription(embedded, l.sub).Status)
		w.nmi.EditSchedule(l.railSub, func(s *nmimock.Schedule) { s.NextBilling = due })
		w.runRenewals()
		require.Zero(t, len(w.nmi.Attempts()), "NMI's schedule must show the next period when OpenRails charges")
	})
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
			require.Zero(t, len(w.nmi.Attempts()), "a charge NMI made is never made again")
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
