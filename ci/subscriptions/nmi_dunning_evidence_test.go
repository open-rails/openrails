//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

func providerDunning(book *billing.DeclaredBilling) {
	declareRecurringAnchor(book)
}

// A paid-through row whose NMI date went stale (OpenRails' own recovery
// charge does not move it) is no failed period: the pull leaves it active.
func TestNMIStaleRosterDateInsidePaidPeriod(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded, providerDunning)
	w.converge()
	end := l.periodEnd()
	w.nmi.EditSchedule(l.railSub, func(s *nmimock.Schedule) { s.NextBilling = w.clock.Now().Add(-2 * day) })
	w.pull()
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(end))
	require.Nil(t, nextRetry(sub))
	w.runRenewals()
	require.Zero(t, len(w.nmi.Attempts()))
}

// A decline first seen after its grace would have ended still enters dunning:
// the parked row is read at once and finds it. Grace runs from discovery and
// retries from the decline, so the overdue first retry recovers the period.
func TestNMIDeclineDiscoveredLateIsDunned(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded, providerDunning)
	w.converge()
	end := l.periodEnd()
	l.providerRenewal(false) // NMI declines at the boundary; no webhook arrives
	w.advance(end.Sub(w.clock.Now()) + 3*day)
	w.converge()
	now := w.clock.Now()
	require.Eventually(t, func() bool { return nextRetry(w.subscription(embedded, l.sub)) != nil }, 30*time.Second, 50*time.Millisecond, "the park's read finds the decline")
	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status)
	grace := w.graceEnds(l.sub)
	require.NotNil(t, grace)
	require.WithinDuration(t, now.Add(48*time.Hour), *grace, time.Second, "grace runs from discovery")
	require.NotNil(t, nextRetry(sub))
	require.WithinDuration(t, end.Add(48*time.Hour), *nextRetry(sub), time.Second, "first retry is the schedule's +2d from the decline")
	require.Zero(t, len(w.nmi.Attempts()))

	w.refreshProviders()
	w.settleCollectionScans()
	w.runRenewals()
	sub = w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.True(t, sub.CurrentPeriodEndsAt.Equal(end.Add(monthHours*time.Hour)))
	require.Equal(t, 1, len(w.nmi.Attempts()), "one recovery charge")
}

// NMI renews at the boundary, then declines the next period. The renewal
// pays one period only; the decline behind it is dunned, not skipped.
func TestNMIRenewalThenDeclineDunsTheUnpaidPeriod(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded, providerDunning)
	w.converge()
	end := l.periodEnd()
	paid := end.Add(monthHours * time.Hour)
	l.providerRenewal(true) // its webhook is lost
	w.advance(paid.Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))

	sub := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionPastDue, sub.Status, "the declined period is dunned")
	require.True(t, sub.CurrentPeriodEndsAt.Equal(paid), "one paid period granted (%s vs %s)", sub.CurrentPeriodEndsAt, paid)
	require.NotNil(t, nextRetry(sub))
	require.WithinDuration(t, paid.Add(48*time.Hour), *nextRetry(sub), time.Second, "retries run from NMI's decline")
	require.Len(t, completed(w.payments(embedded, l.c.id)), 2, "the initial charge and NMI's renewal")
	require.Zero(t, len(w.nmi.Attempts()))
}

// A subscription that appears locally while the pull is reading NMI's roster
// is absent from that roster without being gone: it is never canceled.
func TestNMIPullIgnoresRowsCreatedDuringTheFetch(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	importLegacy(t, w, "nmi", embedded)
	late := importLegacy(t, w, "nmi", embedded)
	w.converge()
	hide := func(hidden bool) {
		deleted := "NULL"
		if hidden {
			deleted = "now()"
		}
		_, err := w.pool.Exec(t.Context(), w.sql(`UPDATE billing.subscriptions SET deleted_at = `+deleted+` WHERE id = $1`), late.sub.UUID())
		require.NoError(t, err)
		w.nmi.EditSchedule(late.railSub, func(s *nmimock.Schedule) { s.Deleted = hidden })
	}
	hide(true)
	g := w.nmi.hold(newGate(func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v5/subscriptions")
	}, true))
	g.served = true
	res, err := w.jobs.Insert(t.Context(), refreshMerchant{MerchantID: w.client[embedded].MerchantID().UUID()}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	select {
	case <-g.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the pull never read the roster")
	}
	hide(false)
	close(g.release)
	w.waitJob(res.Job.ID)
	w.nmi.unhold()

	sub := w.subscription(embedded, late.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status, "a row the roster could not have listed is not canceled")
	require.Zero(t, w.nmi.ScheduleDeletes(late.railSub))
}
