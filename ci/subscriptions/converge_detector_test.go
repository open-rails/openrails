//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/hosttools"
)

type lifeRow struct {
	status      string
	periodEnd   time.Time
	nextRetryAt *time.Time
}

func (w *world) lifeRow(sub billing.SubscriptionID) lifeRow {
	w.t.Helper()
	var r lifeRow
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status::text, current_period_ends_at, next_retry_at FROM billing.subscriptions WHERE id = $1`), sub.UUID()).
		Scan(&r.status, &r.periodEnd, &r.nextRetryAt))
	return r
}

// decide stands in for another writer's lifecycle decision on the row.
func (w *world) decide(sub billing.SubscriptionID, set string, args ...any) {
	w.t.Helper()
	_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.subscriptions SET `+set+`, lifecycle_rev = lifecycle_rev + 1 WHERE id = $1`), append([]any{sub.UUID()}, args...)...)
	require.NoError(w.t, err)
}

// Convergence detects and asks, never deciding a period or a retry: overdue
// rows become unverified with period and retry untouched, a row another writer
// moved meanwhile is left as is, and the reported backlog clears as rows resolve.
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
			_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.subscriptions SET next_retry_at = $2 WHERE id = $1`), retried.sub.UUID(), retryAt)
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
// derive.subscription.missing race).
func TestLegacyImportConvergesAtCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var res []hosttools.ConvergeMerchantResult
	remove := failpoint.Set(func(ctx context.Context, s failpoint.Site) error {
		if s.Point != failpoint.Committed || s.Kind != "billing_import" {
			return nil
		}
		r, err := hosttools.Converge(context.Background(), engine.Graph(w.rt), w.client[embedded].MerchantID())
		res = append(res, r)
		return err
	})
	defer remove()
	l := importLegacy(t, w, "nmi", embedded)
	remove()
	require.NotEmpty(t, res, "convergence ran at the import's commit")
	for _, r := range res {
		require.Zero(t, r.Findings, "%+v", r)
	}
	require.Empty(t, w.findingsAbout(l.sub))
	require.True(t, l.c.entitled(l.ent))
}

// sweep runs the scheduled converge sweep once.
func (w *world) sweep() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), convergeSweep{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	w.waitJob(res.Job.ID)
}

// The kill switch holds only the retractions the sweep would make on its own.
// Off, the sweep still detects and notifies: a staff revoke's access-ended
// notice is queued, and a revoke whose effect never reached its window is a
// finding naming the switch, the window left open. On, the sweep retracts it.
func TestKillSwitchHoldsOnlySweepRetractions(t *testing.T) {
	t.Parallel()
	w := newWorld(t) // a fresh deployment: the switch is off
	key := "held:" + uuid.NewString()[:8]
	product := w.giftProduct(key)
	revoked, drifted := w.newCustomer(), w.newCustomer()
	access := revoked.grant(product, nil, nil)
	open := drifted.grant(product, nil, nil)
	require.Equal(t, http.StatusNoContent, w.staffCall(http.MethodPost, "/v1/admin/product-access/"+access.ID.String()+"/revoke", map[string]any{"reason": "support"}, nil))
	var grant uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`INSERT INTO billing.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, event, supersedes_id, starts_at, reason)
		SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, 'revoke', g.id, $2, 'support'
		FROM billing.grants g JOIN billing.product_access pa ON pa.merchant_id = g.merchant_id AND pa.grant_id = g.id WHERE pa.id = $1
		RETURNING supersedes_id`), open.ID.UUID(), w.clock.Now()).Scan(&grant))
	w.advance(time.Minute)
	excess := func() (status string, evidence map[string]any) {
		t.Helper()
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status, evidence->'local' FROM billing.reconciliation_findings
			WHERE finding_type = 'derive.grant_effect.excess' AND subject_key = $1`), "grant_effect:"+grant.String()).Scan(&status, &evidence))
		return status, evidence
	}

	w.sweep()
	require.Equal(t, 1, w.accessEndedNotices(revoked.id), "the revoke's notice is queued with the switch off")
	status, evidence := excess()
	require.Equal(t, "reconcile_required", status)
	require.Equal(t, "kill_switch", evidence["held_by"])
	require.Contains(t, evidence["held_reason"], "kill switch is OFF")
	require.True(t, drifted.entitled(key), "the sweep retracted nothing")
	require.Zero(t, w.accessEndedNotices(drifted.id))

	w.armDestructive()
	w.sweep()
	status, evidence = excess()
	require.Equal(t, "auto_fixed", status)
	require.NotContains(t, evidence, "held_by")
	require.False(t, drifted.entitled(key), "the switch on, the sweep retracts")
	require.Equal(t, 1, w.accessEndedNotices(drifted.id))
	require.Equal(t, 1, w.accessEndedNotices(revoked.id))
}
