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
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/providerrecovery"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

type pausedRecoveryStore struct {
	*intents.Store
	entered, release chan struct{}
	once             sync.Once
	committed        *atomic.Bool
	skipInline       bool
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
	if s.skipInline {
		return nil
	}
	if !s.committed.Load() {
		return fmt.Errorf("recovery wake ran before the normal dispatcher's successor committed")
	}
	return s.Store.WakeRecoveryHeld(ctx, id, now)
}

// Exact ordering: Verify observed old coverage, completion skips its live lease,
// then Verify commits a future hold. Both actors use real PostgreSQL transitions;
// the wrapper pauses a transition, never fabricates coverage or payment evidence.
func TestProviderRecoveryCompletionRacesLiveVerifier(t *testing.T) {
	for _, holdFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion_commits_first", true: "hold_commits_first"}[holdFirst], func(t *testing.T) { testRecoveryWakeCommitOrder(t, holdFirst, false) })
	}
	t.Run("crash_after_hold_commit", func(t *testing.T) { testRecoveryWakeCommitOrder(t, false, true) })
}

func testRecoveryWakeCommitOrder(t *testing.T, holdFirst, crashAfterHold bool) {
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
	store.skipInline = crashAfterHold
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
	refreshJob := &river.Job[riverjobs.ProviderRefreshMerchantArgs]{Args: riverjobs.ProviderRefreshMerchantArgs{MerchantID: w.client[embedded].MerchantID().UUID()}}
	if holdFirst {
		inserter := &pausedRecoveryInserter{RiverJobInserter: w.jobs, entered: make(chan struct{}), release: make(chan struct{})}
		rt.DB.SetRiverJobInserter(inserter)
		var releaseCompletion sync.Once
		finishCompletion := func() { releaseCompletion.Do(func() { close(inserter.release) }) }
		t.Cleanup(finishCompletion)
		refreshDone := make(chan error, 1)
		go func() { refreshDone <- refresh.Work(t.Context(), refreshJob) }()
		select {
		case <-inserter.entered:
		case <-time.After(10 * time.Second):
			t.Fatal("completion did not reach its pre-commit barrier")
		}
		unblock()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("hold did not commit before completion")
		}
		held, err := store.Get(ctx, id)
		require.NoError(t, err)
		require.True(t, held.NextAttemptAt.After(w.clock.Now()), "verifier's post-commit read still sees the old coverage")
		finishCompletion()
		select {
		case err := <-refreshDone:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("completion did not commit")
		}
	} else {
		require.NoError(t, refresh.Work(t.Context(), refreshJob))
		before, err := store.Get(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, before.LeaseExpiresAt, "completion did not steal the verifier's lease")
		if crashAfterHold {
			// The durable completion consumer has already run while Verify owns
			// the lease. Only the hold's own durable successor can help next.
			due := &riverjobs.DunningWorker{DB: rt.DB, Config: rt.Config, Clock: w.clock, Intents: rt.IntentRunner(), EngineCollections: rt.MoneyService, NMIResolver: rt.CollectionResolver}
			require.NoError(t, due.Work(t.Context(), &river.Job[riverjobs.DunningArgs]{Args: riverjobs.DunningArgs{MerchantID: w.client[embedded].MerchantID().UUID()}}))
		}
		unblock()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("verifier hold did not commit")
		}
	}
	require.NoError(t, providerrecovery.CheckPSP(ctx, rt.DB, w.client[embedded].MerchantID().UUID(), w.psp["nmi"].UUID(), w.clock.Now()))
	after, err := store.Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, after.Status)
	require.Nil(t, after.LeaseExpiresAt)
	if !holdFirst && !crashAfterHold {
		require.False(t, after.NextAttemptAt.After(w.clock.Now()), "post-commit recheck expedites the now-unleased operation")
	} else {
		require.True(t, after.NextAttemptAt.After(w.clock.Now()), "the durable consumer must handle the old delay")
	}
	var runnable int
	require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.provider_operation' AND args->>'intent_id'=$1 AND scheduled_at<=$2 AND state IN ('available','scheduled')`), id.String(), time.Now()).Scan(&runnable))
	require.Positive(t, runnable, "the hold transaction persists an immediate readiness check even if its caller dies")
	if crashAfterHold {
		// Drive the ordinary dispatcher for that durable successor without any
		// other running workers or another provider-refresh pass.
		worker := &riverjobs.ProviderOperationWorker{DB: rt.DB, Config: rt.Config, Clock: w.clock, Registry: runner.Registry}
		job := &river.Job[intents.OperationArgs]{Args: intents.OperationArgs{MerchantID: w.client[embedded].MerchantID().UUID(), IntentID: id}}
		for range 3 {
			require.NoError(t, worker.Work(t.Context(), job))
			current, err := store.Get(ctx, id)
			require.NoError(t, err)
			if intents.OperationTerminal(current.Status) {
				break
			}
		}
		current, err := store.Get(ctx, id)
		require.NoError(t, err)
		require.Equal(t, intents.StatusSucceeded, current.Status, "durable hold successor survives its caller's missing recheck")
	}
	w.restart()
	require.Eventually(t, func() bool { return e.periodEnd().After(end) }, 20*time.Second, 30*time.Millisecond, "ordinary dispatch resumes without a manual wake")
	require.Len(t, w.nmi.Attempts(), 2)
	require.Len(t, w.nmi.Ledger(""), 2)
}

// Pause the final real insert while the completion transaction is still open.
// Other inserts, including the verifier's successor, use the actual River client.
type pausedRecoveryInserter struct {
	db.RiverJobInserter
	entered, release chan struct{}
	once             sync.Once
}

func (p *pausedRecoveryInserter) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	result, err := p.RiverJobInserter.InsertTx(ctx, tx, args, opts)
	if err != nil {
		return result, err
	}
	if invoice, ok := args.(riverjobs.InvoiceArgs); ok && invoice.UseMonthlyFloor {
		p.once.Do(func() { close(p.entered) })
		select {
		case <-p.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return result, nil
}
