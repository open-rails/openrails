//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/embed/operator"
	"github.com/open-rails/openrails/internal/failpoint"
)

type lifeRow struct {
	status      string
	periodEnd   time.Time
	nextRetryAt *time.Time
}

func (w *world) lifeRow(sub openrails.SubscriptionID) lifeRow {
	w.t.Helper()
	var r lifeRow
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status::text, current_period_ends_at, next_retry_at FROM openrails.subscriptions WHERE id = $1`), sub.UUID()).
		Scan(&r.status, &r.periodEnd, &r.nextRetryAt))
	return r
}

// decide stands in for another writer's lifecycle decision on the row.
func (w *world) decide(sub openrails.SubscriptionID, set string, args ...any) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE openrails.subscriptions SET `+set+`, lifecycle_rev = lifecycle_rev + 1 WHERE id = $1`), append([]any{sub.UUID()}, args...)...)
	require.NoError(w.t, err)
}

// Convergence detects and asks; it never decides a period or a retry
// (#1089 §8). An overdue renewal and a dunning window closed with nothing
// scheduled become unverified with their period and retry untouched; a row
// that moved between detection and repair is left as the other writer left
// it; the unverified backlog is reported and clears once the rows resolve.
func TestConvergeDetectsWithoutDeciding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	tier := w.bookTier("monthly", 999, 30)
	m := w.mirrorBook(embedded, tier, 4)
	overdue, stalled, renewed, retried := m[0], m[1], m[2], m[3]
	end := *w.subscription(embedded, overdue.sub).CurrentPeriodEndsAt
	w.advance(end.Sub(w.clock.Now()) + 3*day)
	now := w.clock.Now()
	for _, l := range []*legacy{stalled, retried} {
		w.decide(l.sub, `status = 'past_due', grace_ends_at = $2, next_retry_at = NULL`, now.Add(-time.Hour))
	}
	retryAt := now.Add(day).UTC().Truncate(time.Second)
	moved := map[uuid.UUID]bool{}
	var mu sync.Mutex
	// The hook is process-wide: the runtime's own converge sweep can reach it
	// first, under a job context that may end. The move is made on the test's
	// context so it lands whichever pass reaches the row first.
	remove := failpoint.Set(func(_ context.Context, s failpoint.Site) error {
		mu.Lock()
		defer mu.Unlock()
		if s.Point != failpoint.BeforeRepair || moved[s.Subscription] {
			return nil
		}
		switch s.Subscription {
		case renewed.sub.UUID():
			moved[s.Subscription] = true
			w.decide(renewed.sub, `current_period_ends_at = current_period_ends_at + interval '30 days'`)
		case retried.sub.UUID():
			moved[s.Subscription] = true
			_, err := w.pool.Exec(t.Context(), w.q(`UPDATE openrails.subscriptions SET next_retry_at = $2 WHERE id = $1`), retried.sub.UUID(), retryAt)
			return err
		}
		return nil
	})
	defer remove()
	w.converge()
	remove()
	mu.Lock()
	require.Len(t, moved, 2, "both moves ran between detection and repair")
	mu.Unlock()

	for _, l := range []*legacy{overdue, stalled} {
		r := w.lifeRow(l.sub)
		require.Equal(t, "unverified", r.status, "a clock reading only asks the provider")
		require.True(t, r.periodEnd.Equal(end), "the period is never moved")
		require.Nil(t, r.nextRetryAt, "no retry is invented")
		require.True(t, l.c.entitled(l.ent), "uncertainty keeps access")
	}
	r := w.lifeRow(renewed.sub)
	require.Equal(t, "active", r.status, "a renewal that landed first stands")
	require.True(t, r.periodEnd.Equal(end.Add(30*day)))
	r = w.lifeRow(retried.sub)
	require.Equal(t, "past_due", r.status, "a retry scheduled meanwhile stands")
	require.True(t, r.periodEnd.Equal(end))
	require.NotNil(t, r.nextRetryAt)
	require.True(t, r.nextRetryAt.Equal(retryAt))
	require.Zero(t, len(w.nmi.Attempts()), "convergence charges nobody")

	// The next pass reports the backlog and the funnel; renewals NMI
	// charged clear it.
	w.settle()
	w.converge()
	require.Len(t, w.openFindings("life.unverified.backlog"), 1)
	require.Len(t, w.openFindings("life.dunning.funnel"), 1)
	for _, l := range []*legacy{overdue, stalled} {
		require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
		require.Equal(t, "active", w.lifeRow(l.sub).status)
	}
	w.converge()
	require.Empty(t, w.openFindings("life.unverified.backlog"), "the backlog clears with its rows")
	require.Contains(t, w.findingsAbout(overdue.sub), "life.subscription.renewal_overdue:auto_fixed")
	require.Zero(t, len(w.nmi.Attempts()))
}

// An import commits its members together with their access, so a converge
// that runs the moment the import commits finds nothing to repair (the
// derive.subscription.missing race behind the takeover-convergence flake).
func TestLegacyImportConvergesAtCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var res []operator.ConvergeResult
	remove := failpoint.Set(func(ctx context.Context, s failpoint.Site) error {
		if s.Point != failpoint.Committed || s.Kind != "billing_import" {
			return nil
		}
		r, err := operator.New(w.rt).Converge(context.Background(), w.client[embedded].MerchantID())
		res = append(res, r)
		return err
	})
	defer remove()
	l := importTakeoverLegacy(t, w, embedded)
	remove()
	require.NotEmpty(t, res, "convergence ran at the import's commit")
	for _, r := range res {
		require.Zero(t, r.Findings, "%+v", r)
	}
	require.Empty(t, w.findingsAbout(l.sub))
	require.True(t, l.c.entitled(l.ent))
}
