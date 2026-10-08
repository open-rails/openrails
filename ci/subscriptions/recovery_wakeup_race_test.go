//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/providerrecovery"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

type pausedRecoveryStore struct {
	*intents.Store
	entered, release chan struct{}
	once             sync.Once
	committed        *atomic.Bool
}

func (s *pausedRecoveryStore) MarkUnknown(ctx context.Context, id uuid.UUID, at time.Time, reason string, evidence map[string]any) error {
	if evidence["recovery_held"] == true {
		s.once.Do(func() { close(s.entered) })
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.MarkUnknown(ctx, id, at, reason, evidence)
}

func (s *pausedRecoveryStore) WakeRecoveryHeld(ctx context.Context, id uuid.UUID, now time.Time) error {
	if !s.committed.Load() {
		return fmt.Errorf("recovery wake ran before the normal dispatcher's successor committed")
	}
	return s.Store.WakeRecoveryHeld(ctx, id, now)
}

// Exact ordering: Verify observed old coverage, completion skips its live lease,
// then Verify commits a future hold. Both actors use real PostgreSQL transitions;
// the wrapper pauses a transition, never fabricates coverage or payment evidence.
func TestProviderRecoveryCompletionRacesLiveVerifier(t *testing.T) {
	w := prepareWorld(t, 12)
	w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
	w.start()
	e := enrollEvery(t, w, "nmi", embedded, 8)
	end := e.periodEnd()
	w.advance(end.Add(time.Hour).Sub(w.clock.Now()))
	var unavailable atomic.Bool
	unavailable.Store(true)
	w.nmi.FailRequests(func(r *http.Request) bool {
		form, err := url.ParseQuery(readBody(r))
		return unavailable.Load() && err == nil && strings.HasSuffix(r.URL.Path, "/query.php") && form.Get("report_type") == "transaction" && form.Get("order_id") == "" && form.Get("transaction_id") == ""
	}, http.StatusServiceUnavailable, 100)
	w.runRenewals()
	var id uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='subscription_collection'`), e.sub.UUID()).Scan(&id))
	require.NoError(t, w.jobs.Stop(t.Context()))
	w.advance(intents.ParkRetryInterval + time.Second)
	unavailable.Store(false)
	rt := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	require.ErrorIs(t, providerrecovery.CheckPSP(ctx, rt.DB, w.client[embedded].MerchantID().UUID(), w.psp["nmi"].UUID(), w.clock.Now()), providerrecovery.ErrPending)
	var committed atomic.Bool
	store := &pausedRecoveryStore{Store: intents.NewStore(rt.DB), entered: make(chan struct{}), release: make(chan struct{}), committed: &committed}
	runner := rt.IntentRunner()
	runner.Store = store
	runner.OnSuccessorCommitted = func(uuid.UUID, uuid.UUID) { committed.Store(true) }
	done := make(chan error, 1)
	go func() { _, err := runner.VerifyByID(ctx, id); done <- err }()
	var release sync.Once
	unblock := func() { release.Do(func() { close(store.release) }) }
	t.Cleanup(unblock)
	select {
	case <-store.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("verifier did not observe the old coverage")
	}
	refresh := &riverjobs.ProviderRefreshWorker{DB: rt.DB, Config: rt.Config, Clock: w.clock, Merchants: rt.Merchants, NMIClients: rt.NMIClients, StripeClients: rt.StripeClients}
	require.NoError(t, refresh.Work(t.Context(), &river.Job[riverjobs.ProviderRefreshMerchantArgs]{Args: riverjobs.ProviderRefreshMerchantArgs{MerchantID: w.client[embedded].MerchantID().UUID()}}))
	require.NoError(t, providerrecovery.CheckPSP(ctx, rt.DB, w.client[embedded].MerchantID().UUID(), w.psp["nmi"].UUID(), w.clock.Now()))
	before, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, before.LeaseExpiresAt, "completion did not steal the verifier's lease")
	unblock()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("verifier hold did not commit")
	}
	after, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, after.Status)
	require.Nil(t, after.LeaseExpiresAt)
	require.False(t, after.NextAttemptAt.After(w.clock.Now()), "post-commit recheck durably expedites the now-unleased operation")
	var runnable int
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.provider_operation' AND args->>'intent_id'=$1 AND scheduled_at<=$2 AND state IN ('available','scheduled')`), id.String(), w.clock.Now()).Scan(&runnable))
	require.Positive(t, runnable, "a timestamp update alone cannot wake a sleeping River job")
	w.restart()
	require.Eventually(t, func() bool { return e.periodEnd().After(end) }, 20*time.Second, 30*time.Millisecond, "ordinary dispatch resumes without a manual wake")
	require.Len(t, w.nmi.Attempts(), 2)
	require.Len(t, w.nmi.Ledger(""), 2)
}
