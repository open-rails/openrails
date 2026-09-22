//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// Worker-stop defers run before this cleanup; remove only this fixture's
// JSON-addressed jobs before seedIntentMerchant removes its ledger rows.
func seedTransitionMerchant(t *testing.T) intentMerchant {
	t.Helper()
	m := seedIntentMerchant(t)
	t.Cleanup(func() {
		_, err := m.pool.Exec(context.Background(), "DELETE FROM "+pgx.Identifier{config.RiverSchema, "river_job"}.Sanitize()+" WHERE args->>'merchant_id'=$1", m.id.String())
		require.NoError(t, err)
	})
	return m
}

type inlineUnknownTransitionHandler struct{ entered, release chan struct{} }

func (*inlineUnknownTransitionHandler) Type() string { return "test_inline_unknown" }
func (*inlineUnknownTransitionHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.Relevance{Applicable: true}, nil
}
func (h *inlineUnknownTransitionHandler) Execute(ctx context.Context, _ gen.OpenrailsRailIntent) intents.Outcome {
	close(h.entered)
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return intents.Ambiguous("synthetic lost reply")
}
func (*inlineUnknownTransitionHandler) Verify(context.Context, gen.OpenrailsRailIntent) intents.Outcome {
	return intents.Ambiguous("synthetic unknown receipt")
}
func (*inlineUnknownTransitionHandler) Backoff(int32) time.Duration { return time.Second }

func TestInlineUnknownTransitionRearmsSleepingRiverJob(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	m := seedTransitionMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	h := &inlineUnknownTransitionHandler{entered: make(chan struct{}), release: make(chan struct{})}
	registry := intents.NewRegistry(h)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ProviderOperationWorker{DB: d, Registry: registry, Config: cfg})
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 1}}, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
	mctx := merchant.WithID(ctx, merchant.ID(m.id))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	store := intents.NewStore(d)
	row, err := store.Enqueue(mctx, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: h.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: time.Now()})
	require.NoError(t, err)
	runner := &intents.Runner{Store: store, Registry: registry, Config: cfg}
	finished := make(chan error, 1)
	go func() { _, err := runner.ExecuteByID(mctx, row.ID); finished <- err }()
	<-h.entered
	events, unsub := client.Subscribe(river.EventKindJobSnoozed)
	defer unsub()
	require.NoError(t, client.Start(ctx))
	defer func() { require.NoError(t, client.Stop(context.Background())) }()
	for {
		event := <-events
		var args intents.OperationArgs
		require.NoError(t, json.Unmarshal(event.Job.EncodedArgs, &args))
		if args.IntentID == row.ID {
			break
		}
	}
	close(h.release)
	require.NoError(t, <-finished)
	current, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
	var nextWake time.Time
	require.NoError(t, d.Pool().QueryRow(ctx, "SELECT min(scheduled_at) FROM public.river_job WHERE args->>'intent_id'=$1 AND state NOT IN ('completed','cancelled','discarded')", row.ID.String()).Scan(&nextWake))
	t.Logf("ledger verification due=%s earliest River wake=%s delay=%s", current.NextAttemptAt, nextWake, nextWake.Sub(current.NextAttemptAt))
	require.True(t, nextWake.Equal(current.NextAttemptAt), "inline transition and River wake must commit the same instant")
}

var errTransitionWakeInsert = errors.New("injected failure after inserting transition wake")

type transitionInsertControl struct {
	client *river.Client[pgx.Tx]
	fail   atomic.Bool
	calls  atomic.Int64
	conn   *pgx.Conn
	t      *testing.T
}

func (c *transitionInsertControl) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	if c.conn != nil {
		require.Same(c.t, c.conn, tx.Conn(), "transition wake stays in ambient transaction/savepoint")
	}
	c.calls.Add(1)
	result, err := c.client.InsertTx(ctx, tx, args, opts)
	if err != nil {
		return nil, err
	}
	if c.fail.Load() {
		return nil, errTransitionWakeInsert
	}
	return result, nil
}

func TestOperationTransitionsAtomicallyCommitTheirWake(t *testing.T) {
	for _, kind := range []string{"unknown", "retryable", "park", "submitted_park", "release"} {
		t.Run(kind, func(t *testing.T) {
			ctx := t.Context()
			m := seedTransitionMerchant(t)
			d := dbtest.OpenOneConnAppDB(t)
			owner := dbtest.SharedSuperuserPGXPool(t)
			producer, err := river.NewClient(riverpgxv5.New(owner), &river.Config{Schema: config.RiverSchema})
			require.NoError(t, err)
			d.SetRiverJobInserter(producer)
			psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
			scoped := merchant.WithID(ctx, merchant.ID(m.id))
			store := intents.NewStore(d)
			now := time.Date(2025, 1, 2, 12, 0, 0, 123456000, time.FixedZone("business", -7*3600))
			due := now.Add(17 * time.Minute)
			row, err := store.Enqueue(scoped, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: "test_transition", Payload: map[string]string{"frozen": "accepted"}, IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: now.Add(time.Hour)})
			require.NoError(t, err)
			if kind == "submitted_park" {
				m.exec(t, `UPDATE billing.rail_intents SET intent_type='initial_membership',result_evidence='{"initial_submitted":true,"diagnostic":"accepted"}' WHERE id=$1`, row.ID)
			}
			_, claimed, err := store.ClaimByID(scoped, row.ID, now, now.Add(2*time.Hour))
			require.NoError(t, err)
			require.True(t, claimed)
			if kind == "release" {
				m.exec(t, `UPDATE billing.rail_intents SET status='unknown_needs_verify' WHERE id=$1`, row.ID)
			}
			before, err := store.Get(scoped, row.ID)
			require.NoError(t, err)
			if kind == "release" {
				due = before.NextAttemptAt
			}
			jobs := func() int {
				var n int
				require.NoError(t, owner.QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE args->>'intent_id'=$1`, row.ID.String()).Scan(&n))
				return n
			}
			transition := func(ctx context.Context, s *intents.Store) error {
				switch kind {
				case "release":
					changed, err := s.ReleaseUnknownClaim(ctx, row.ID)
					if err != nil {
						require.False(t, changed, "failed insertion cannot report a committed release")
					}
					return err
				case "unknown":
					return s.MarkUnknown(ctx, row.ID, due, "awaiting readback", map[string]any{"diagnostic": "accepted"})
				case "retryable":
					return s.MarkFailedRetryable(ctx, row.ID, due, "permitted retry")
				default:
					return s.Park(ctx, row.ID, due, "write gate blocked")
				}
			}
			if kind == "release" {
				m.exec(t, `UPDATE billing.rail_intents SET status='in_flight' WHERE id=$1`, row.ID)
				changed, err := store.ReleaseUnknownClaim(scoped, row.ID)
				require.NoError(t, err)
				require.False(t, changed, "resolver release cannot steal an execution claim")
				stillClaimed, err := store.Get(scoped, row.ID)
				require.NoError(t, err)
				require.Equal(t, before.ClaimedUntil, stillClaimed.ClaimedUntil)
				require.Equal(t, 1, jobs())
				m.exec(t, `UPDATE billing.rail_intents SET status='unknown_needs_verify' WHERE id=$1`, row.ID)
			}
			control := &transitionInsertControl{client: producer, t: t}
			control.fail.Store(true)
			d.SetRiverJobInserter(control)
			require.ErrorIs(t, transition(scoped, store), errTransitionWakeInsert)
			after, err := store.Get(scoped, row.ID)
			require.NoError(t, err)
			require.Equal(t, before, after, "failed followup insert rolls back state, lease, evidence and schedule")
			require.Equal(t, 1, jobs(), "original recoverable job remains, failed new job disappears")
			// Failure inside a nested savepoint must not poison the host transaction.
			require.NoError(t, d.MerchantTx(scoped, func(ctx context.Context, tx pgx.Tx) error {
				control.conn = tx.Conn()
				require.ErrorIs(t, transition(ctx, intents.NewStore(db.NewWithPgxTx(tx))), errTransitionWakeInsert)
				var value int
				require.NoError(t, tx.QueryRow(ctx, "SELECT 42").Scan(&value))
				require.Equal(t, 42, value)
				return nil
			}))
			control.conn = nil
			after, err = store.Get(scoped, row.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, 1, jobs())
			control.fail.Store(false)
			rollback := errors.New("host rollback after successful transition")
			err = d.MerchantTx(scoped, func(ctx context.Context, tx pgx.Tx) error {
				control.conn = tx.Conn()
				require.NoError(t, transition(ctx, intents.NewStore(db.NewWithPgxTx(tx))))
				require.Equal(t, 1, jobs(), "worker cannot observe uncommitted wake")
				return rollback
			})
			require.ErrorIs(t, err, rollback)
			control.conn = nil
			after, err = store.Get(scoped, row.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, 1, jobs())
			require.NoError(t, transition(scoped, store))
			after, err = store.Get(scoped, row.ID)
			require.NoError(t, err)
			expected := intents.StatusUnknownNeedsVerify
			if kind == "retryable" {
				expected = intents.StatusFailedRetryable
			}
			if kind == "park" {
				expected = intents.StatusPending
			}
			require.Equal(t, expected, after.Status)
			require.Nil(t, after.ClaimedUntil)
			require.Equal(t, before.Payload, after.Payload)
			if kind == "submitted_park" {
				require.Equal(t, before.ResultEvidence, after.ResultEvidence)
			}
			require.True(t, due.Equal(after.NextAttemptAt))
			require.Equal(t, 2, jobs(), "one successful transition creates exactly one followup")
			var scheduled time.Time
			var raw []byte
			require.NoError(t, owner.QueryRow(ctx, `SELECT scheduled_at,args FROM public.river_job WHERE args->>'intent_id'=$1 ORDER BY id DESC LIMIT 1`, row.ID.String()).Scan(&scheduled, &raw))
			require.True(t, after.NextAttemptAt.Equal(scheduled), "business-clock due time is not replaced by wall clock")
			var args map[string]any
			require.NoError(t, json.Unmarshal(raw, &args))
			require.Equal(t, map[string]any{"merchant_id": m.id.String(), "intent_id": row.ID.String()}, args)
			calls := control.calls.Load()
			if kind == "release" {
				changed, err := store.ReleaseUnknownClaim(scoped, row.ID)
				require.NoError(t, err)
				require.False(t, changed, "already released is a no-op")
			}
			other := merchant.WithID(ctx, merchant.ID(uuid.New()))
			err = transition(other, store)
			if kind == "retryable" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			unchanged, err := store.Get(scoped, row.ID)
			require.NoError(t, err)
			require.Equal(t, after, unchanged)
			m.exec(t, `UPDATE billing.rail_intents SET status='succeeded',executed_at=now() WHERE id=$1`, row.ID)
			err = transition(scoped, store)
			if kind == "retryable" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, calls, control.calls.Load(), "zero-row transitions cannot touch the producer")
			require.Equal(t, 2, jobs())
		})
	}
}

type rejectedOperatorHandler struct {
	entered, release, verifyEntered, verifyRelease chan struct{}
	executed, verified                             atomic.Int32
}

func (*rejectedOperatorHandler) Type() string { return "test_rejected_operator" }
func (*rejectedOperatorHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *rejectedOperatorHandler) Execute(context.Context, gen.OpenrailsRailIntent) intents.Outcome {
	h.executed.Add(1)
	return intents.Ambiguous("unexpected execution")
}
func (h *rejectedOperatorHandler) Verify(ctx context.Context, _ gen.OpenrailsRailIntent) intents.Outcome {
	if h.verified.Add(1) == 1 {
		close(h.verifyEntered)
	}
	select {
	case <-h.verifyRelease:
	case <-ctx.Done():
	}
	return intents.Succeeded(nil)
}
func (*rejectedOperatorHandler) Backoff(int32) time.Duration { return time.Second }
func (h *rejectedOperatorHandler) Resolve(ctx context.Context, _ gen.OpenrailsRailIntent, _ intents.Resolution) (intents.Outcome, error) {
	close(h.entered)
	select {
	case <-h.release:
	case <-ctx.Done():
	}
	return intents.Outcome{}, intents.ErrResolutionRejected
}

func TestRejectedOperatorReleaseRearmsSleepingRiverJobs(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m := seedTransitionMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	h := &rejectedOperatorHandler{entered: make(chan struct{}), release: make(chan struct{}), verifyEntered: make(chan struct{}), verifyRelease: make(chan struct{})}
	registry := intents.NewRegistry(h)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ProviderOperationWorker{DB: d, Registry: registry, Config: cfg})
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 1}}, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
	mctx := merchant.WithID(ctx, merchant.ID(m.id))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	store := intents.NewStore(d)
	now := time.Now().UTC()
	row, err := store.Enqueue(mctx, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: h.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: now})
	require.NoError(t, err)
	_, claimed, err := store.ClaimByID(mctx, row.ID, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.MarkUnknown(mctx, row.ID, now.Add(-time.Minute), "awaiting evidence", map[string]any{"diagnostic": "retained"}))
	before, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	runner := &intents.Runner{Store: store, Registry: registry, Config: cfg}
	finished := make(chan error, 1)
	go func() {
		_, err := runner.Resolve(mctx, row.ID, intents.Resolution{ProviderReference: "synthetic", Actor: "operator", Reason: "qualified review attempt"})
		finished <- err
	}()
	select {
	case <-h.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	events, unsub := client.Subscribe(river.EventKindJobSnoozed, river.EventKindJobCompleted)
	defer unsub()
	require.NoError(t, client.Start(ctx))
	defer func() { cancel(); require.NoError(t, client.Stop(context.Background())) }()
	sleeping := map[int64]bool{}
	for len(sleeping) < 2 {
		select {
		case event := <-events:
			var args intents.OperationArgs
			require.NoError(t, json.Unmarshal(event.Job.EncodedArgs, &args))
			if args.IntentID == row.ID && event.Kind == river.EventKindJobSnoozed {
				sleeping[event.Job.ID] = true
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(h.release)
	require.ErrorIs(t, <-finished, intents.ErrResolutionRejected)
	var nextWake time.Time
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT min(scheduled_at) FROM public.river_job WHERE args->>'intent_id'=$1 AND state NOT IN ('completed','cancelled','discarded')`, row.ID.String()).Scan(&nextWake))
	require.True(t, nextWake.Equal(before.NextAttemptAt), "release must wake the retained due instant, not the obsolete resolver lease")
	select {
	case <-h.verifyEntered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	during, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, before.NextAttemptAt, during.NextAttemptAt)
	require.Equal(t, before.Payload, during.Payload)
	require.Equal(t, before.ResultEvidence, during.ResultEvidence)
	require.EqualValues(t, 0, h.executed.Load(), "operator rejection can only resume verification")
	close(h.verifyRelease)
	require.Eventually(t, func() bool {
		current, err := store.Get(mctx, row.ID)
		return err == nil && current.Status == intents.StatusSucceeded
	}, 5*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 1, h.verified.Load())
}

type unresolvedCycleHandler struct {
	reads           atomic.Int32
	fourth, release chan struct{}
}

func (*unresolvedCycleHandler) Type() string { return "test_unresolved_cycles" }
func (*unresolvedCycleHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (*unresolvedCycleHandler) Execute(context.Context, gen.OpenrailsRailIntent) intents.Outcome {
	return intents.Ambiguous("unexpected send")
}
func (h *unresolvedCycleHandler) Verify(ctx context.Context, _ gen.OpenrailsRailIntent) intents.Outcome {
	if h.reads.Add(1) == 4 {
		close(h.fourth)
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
	return intents.Ambiguous("provider remains unresolved")
}
func (*unresolvedCycleHandler) Backoff(int32) time.Duration { return 0 }

func TestUnresolvedOperationDoesNotGrowActiveJobsEachCycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m := seedTransitionMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	h := &unresolvedCycleHandler{fourth: make(chan struct{}), release: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ProviderOperationWorker{DB: d, Registry: intents.NewRegistry(h), Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}})
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 1}}, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
	mctx := merchant.WithID(ctx, merchant.ID(m.id))
	store := intents.NewStore(d)
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	now := time.Now()
	row, err := store.Enqueue(mctx, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: h.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: now})
	require.NoError(t, err)
	_, claimed, err := store.ClaimByID(mctx, row.ID, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, store.MarkUnknown(mctx, row.ID, now, "initial uncertainty", nil))
	active := func() int {
		var n int
		require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE args->>'intent_id'=$1 AND state NOT IN ('completed','cancelled','discarded')`, row.ID.String()).Scan(&n))
		return n
	}
	initial := active()
	require.NoError(t, client.Start(ctx))
	defer func() { close(h.release); cancel(); require.NoError(t, client.Stop(context.Background())) }()
	select {
	case <-h.fourth:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Logf("active jobs before=%d after three unresolved transitions=%d", initial, active())
	require.Eventually(t, func() bool { return active() <= initial+1 }, 2*time.Second, 10*time.Millisecond, "consumed wakeups must retire after committing a durable successor")
}

type wakeFailureHandler struct{ executed, verified atomic.Int32 }

func (*wakeFailureHandler) Type() string { return "test_wake_insert_failure" }
func (*wakeFailureHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *wakeFailureHandler) Execute(context.Context, gen.OpenrailsRailIntent) intents.Outcome {
	h.executed.Add(1)
	return intents.Ambiguous("synthetic uncertain response")
}
func (h *wakeFailureHandler) Verify(context.Context, gen.OpenrailsRailIntent) intents.Outcome {
	h.verified.Add(1)
	return intents.Succeeded(nil)
}
func (*wakeFailureHandler) Backoff(int32) time.Duration { return time.Second }

func TestFailedSuccessorInsertRetainsLastRecoverableRiverJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m := seedTransitionMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	h := &wakeFailureHandler{}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ProviderOperationWorker{DB: d, Registry: intents.NewRegistry(h), Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}})
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 1}}, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
	mctx := merchant.WithID(ctx, merchant.ID(m.id))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	store := intents.NewStore(d)
	row, err := store.Enqueue(mctx, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: h.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: time.Now()})
	require.NoError(t, err)
	var jobID int64
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT id FROM public.river_job WHERE args->>'intent_id'=$1`, row.ID.String()).Scan(&jobID))
	control := &transitionInsertControl{client: client, t: t}
	control.fail.Store(true)
	d.SetRiverJobInserter(control)
	events, unsub := client.Subscribe(river.EventKindJobSnoozed, river.EventKindJobCompleted)
	defer unsub()
	require.NoError(t, client.Start(ctx))
	defer func() { cancel(); require.NoError(t, client.Stop(context.Background())) }()
	wait := func(kind river.EventKind) {
		for {
			select {
			case event := <-events:
				if event.Job.ID == jobID && event.Kind == kind {
					return
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	wait(river.EventKindJobSnoozed)
	retained, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusInFlight, retained.Status)
	require.NotNil(t, retained.ClaimedUntil)
	var active int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE args->>'intent_id'=$1 AND state NOT IN ('completed','cancelled','discarded')`, row.ID.String()).Scan(&active))
	require.Equal(t, 1, active, "failed successor must not retire the last existing job")
	require.EqualValues(t, 1, h.executed.Load())
	control.fail.Store(false)
	// Model the failed executor's expired lease, using the existing River retry API.
	m.exec(t, `UPDATE billing.rail_intents SET claimed_until=now()-interval '1 second' WHERE id=$1`, row.ID)
	_, err = client.JobRetry(ctx, jobID)
	require.NoError(t, err)
	wait(river.EventKindJobCompleted)
	require.Equal(t, intents.StatusSucceeded, m.statusOf(t, row.ID))
	require.EqualValues(t, 1, h.executed.Load(), "recovery cannot blindly execute again")
	require.EqualValues(t, 1, h.verified.Load())
}
