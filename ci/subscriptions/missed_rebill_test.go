//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/nmimock"
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
func (w *world) missReason(sub billing.SubscriptionID, due time.Time) string {
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

	w.runRenewals() // the due pass may already have run on its own
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
		// Once the period is handed over, NMI's schedule shows it due again.
		// Keyed on the recorded miss, so the due pass may run at any moment.
		handedOver := func(r *http.Request) bool {
			if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/subscriptions/"+l.railSub) {
				return false
			}
			var missed bool
			err := w.pool.QueryRow(r.Context(), w.q(`SELECT EXISTS (SELECT 1 FROM billing.rebill_cycles WHERE subscription_id = $1 AND missed_at IS NOT NULL)`), l.sub.UUID()).Scan(&missed)
			return err == nil && missed
		}
		w.nmi.Intercept(handedOver, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
			res := serve()
			var body map[string]any
			raw, _ := io.ReadAll(res.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				return nil, err
			}
			body["next_billing_date"] = due.UTC().Format(time.DateOnly)
			raw, _ = json.Marshal(body)
			res.Body, res.ContentLength = io.NopCloser(bytes.NewReader(raw)), int64(len(raw))
			return res, nil
		})
		w.watchRebills()
		require.Equal(t, "provider_skipped", w.missReason(l.sub, due))
		w.runRenewals()
		require.Zero(t, len(w.nmi.Attempts()), "NMI's schedule must show the next period when OpenRails charges")
	})
}

// NMI attempted the cycle and its webhook never came, but left no charge to
// record: the merchant voided it, NMI charged days before the schedule's date,
// or the merchant charged the card from NMI's dashboard. The watch records the
// miss with what NMI holds and OpenRails never charges the period (#1113).
func TestNMIAttemptedRebillIsNeverCollected(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reason string
		nmi    func(l *legacy, due time.Time)
	}{
		"voided": {"provider_reversed", func(l *legacy, _ time.Time) { l.w.nmi.Void(l.w.nmi.RenewSchedule(l.railSub, true).TransactionID) }},
		"early": {"provider_unrecorded", func(l *legacy, due time.Time) {
			l.w.nmi.EditSchedule(l.railSub, func(s *nmimock.Schedule) { s.NextBilling = due.Add(-3 * day) })
			l.w.nmi.RenewSchedule(l.railSub, true)
		}},
		"dashboard": {"provider_unrecorded", func(l *legacy, due time.Time) {
			l.w.nmi.SkipSchedule(l.railSub)
			l.w.nmi.AddSale(nmimock.Sale{Vault: l.railCust, OrderID: "dashboard", Amount: "9.99", At: due.Add(time.Hour)})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			if tc.reason == "provider_unrecorded" {
				w.waive("recorded", "NMI's charge is left to the operator's review, unrecorded by design")
			}
			l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
			w.converge()
			due := l.periodEnd()
			tc.nmi(l, due)
			w.advance(due.Sub(w.clock.Now()) + 25*time.Hour)
			w.watchRebills()
			w.runRenewals()
			require.Equal(t, tc.reason, w.missReason(l.sub, due))
			require.Zero(t, len(w.nmi.Attempts()), "OpenRails never charges a period NMI attempted")
			require.NotEqual(t, "past_due", w.subscription(embedded, l.sub).Status)
			findings := w.openFindings("life.rebill.missed")
			require.Len(t, findings, 1)
			var evidence struct {
				Collected bool `json:"collected"`
				Held      []struct {
					TransactionID string `json:"transaction_id"`
				} `json:"nmi_transactions"`
			}
			var raw []byte
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT evidence FROM billing.reconciliation_findings WHERE finding_type = 'life.rebill.missed' AND subject_key = $1`), findings[0]).Scan(&raw))
			require.NoError(t, json.Unmarshal(raw, &evidence))
			require.False(t, evidence.Collected)
			require.Len(t, evidence.Held, 1)
			require.NotEmpty(t, evidence.Held[0].TransactionID)

			w.watchRebills()
			w.runRenewals()
			require.Zero(t, len(w.nmi.Attempts()))
			require.Len(t, w.openFindings("life.rebill.missed"), 1, "one finding per cycle")
		})
	}
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
			read := w.enrichedAttempts(l.c.id)
			require.Len(t, read, 1)
			require.NotNil(t, read[0].EnrichedAt, "the probe's read fills the attempt at once (#1114)")
			require.Equal(t, "411111", str(read[0].BIN))
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
