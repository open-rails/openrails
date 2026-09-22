//go:build integration

package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/riverqueue/river/rivermigrate"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

type transactionAssertingInserter struct {
	client *river.Client[pgx.Tx]
	t      *testing.T
	conn   *pgx.Conn
}

func (s transactionAssertingInserter) InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error) {
	if s.conn != nil {
		require.Same(s.t, s.conn, tx.Conn(), "River must use admission's actual PostgreSQL transaction")
	}
	require.IsType(s.t, intents.OperationArgs{}, args)
	return s.client.InsertTx(ctx, tx, args, opts)
}

func TestOperationAdmissionAtomicallyEnqueuesRiver(t *testing.T) {
	for _, schema := range []string{"public", "host_operation_jobs", "openrails"} {
		t.Run(schema, func(t *testing.T) {
			ctx := t.Context()
			m := seedIntentMerchant(t)
			owner := dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t))
			d := dbtest.OpenOneConnAppDB(t)
			_, err := owner.Pool().Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize())
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = owner.Pool().Exec(context.Background(), "DELETE FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE args->>'merchant_id'=$1", m.id.String())
			})
			migration, err := rivermigrate.New(riverpgxv5.New(owner.Pool()), &rivermigrate.Config{Schema: schema})
			require.NoError(t, err)
			_, err = migration.Migrate(ctx, rivermigrate.DirectionUp, nil)
			require.NoError(t, err)
			var role string
			require.NoError(t, d.Pool().QueryRow(ctx, "SELECT current_user").Scan(&role))
			_, err = owner.Pool().Exec(ctx, "GRANT USAGE ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			_, err = owner.Pool().Exec(ctx, "GRANT ALL ON ALL TABLES IN SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			_, err = owner.Pool().Exec(ctx, "GRANT ALL ON ALL SEQUENCES IN SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			// Intentionally a DIFFERENT pool: InsertTx must still use the caller's tx.
			producer, err := river.NewClient(riverpgxv5.New(owner.Pool()), &river.Config{Schema: schema})
			require.NoError(t, err)
			psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
			mctx := merchant.WithID(ctx, merchant.ID(m.id))
			params := intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: "test_operation", Payload: map[string]string{"permitted": "terms"}, IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: time.Now()}
			var row gen.OpenrailsRailIntent
			countJobs := func(id uuid.UUID) int {
				var n int
				require.NoError(t, owner.Pool().QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE args->>'intent_id'=$1", id.String()).Scan(&n))
				return n
			}
			rollback := errors.New("host rollback")
			err = d.MerchantTx(mctx, func(ctx context.Context, tx pgx.Tx) error {
				d.SetRiverJobInserter(transactionAssertingInserter{producer, t, tx.Conn()})
				// Exercise free transaction reconstruction and nested admission savepoint.
				row, err = intents.NewStore(db.NewWithPgxTx(tx)).Enqueue(ctx, params)
				require.NoError(t, err)
				require.Equal(t, 0, countJobs(row.ID), "uncommitted job must be invisible to a worker")
				return rollback
			})
			require.ErrorIs(t, err, rollback)
			require.Equal(t, 0, countJobs(row.ID))
			var ledgerCount int
			require.NoError(t, m.pool.QueryRow(ctx, "SELECT count(*) FROM billing.rail_intents WHERE id=$1", row.ID).Scan(&ledgerCount))
			require.Zero(t, ledgerCount)
			d.SetRiverJobInserter(producer)
			row, err = intents.NewStore(d).Enqueue(mctx, params)
			require.NoError(t, err)
			require.Equal(t, 1, countJobs(row.ID))
			again, err := intents.NewStore(d).Enqueue(mctx, params)
			require.NoError(t, err)
			require.Equal(t, row.ID, again.ID)
			require.Equal(t, 2, countJobs(row.ID), "duplicate wakeups must not suppress a revived or earlier operation")
			// Unbound host composition must reject admission, not strand a ledger row.
			d.SetRiverJobInserter(nil)
			params.IdempotencyKey = uuid.NewString()
			_, err = intents.NewStore(d).Enqueue(mctx, params)
			require.ErrorContains(t, err, "bound River producer")
			require.NoError(t, m.pool.QueryRow(ctx, "SELECT count(*) FROM billing.rail_intents WHERE idempotency_key=$1", params.IdempotencyKey).Scan(&ledgerCount))
			require.Zero(t, ledgerCount)
		})
	}
}

type crashAfterProviderHandler struct {
	url           string
	writes, reads atomic.Int32
}

func (*crashAfterProviderHandler) Type() string { return "test_provider_mutation" }
func (*crashAfterProviderHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.Relevance{Applicable: true}, nil
}
func (h *crashAfterProviderHandler) Execute(ctx context.Context, _ gen.OpenrailsRailIntent) intents.Outcome {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	h.writes.Add(1)
	panic("process lost after provider accepted, before ledger completion")
}
func (h *crashAfterProviderHandler) Verify(ctx context.Context, _ gen.OpenrailsRailIntent) intents.Outcome {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	h.reads.Add(1)
	if resp.StatusCode != 200 {
		return intents.Ambiguous("provider has not confirmed")
	}
	return intents.Succeeded(map[string]any{"provider_receipt": "accepted_once"})
}
func (*crashAfterProviderHandler) Backoff(int32) time.Duration { return time.Second }

func TestOperationRiverRecoversPostProviderCrashWithoutResubmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m := seedIntentMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	var providerWrites atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			providerWrites.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer provider.Close()
	handler := &crashAfterProviderHandler{url: provider.URL}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ProviderOperationWorker{DB: d, Registry: intents.NewRegistry(handler), Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}})
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 1}}, FetchCooldown: time.Millisecond * 10, FetchPollInterval: time.Millisecond * 20})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	row, err := intents.NewStore(d).Enqueue(merchant.WithID(ctx, merchant.ID(m.id)), intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: handler.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: time.Now()})
	require.NoError(t, err)
	var jobID int64
	require.NoError(t, d.Pool().QueryRow(ctx, "SELECT id FROM "+pgx.Identifier{config.RiverSchema, "river_job"}.Sanitize()+" WHERE args->>'intent_id'=$1", row.ID.String()).Scan(&jobID))
	events, unsubscribe := client.Subscribe(river.EventKindJobSnoozed, river.EventKindJobCompleted)
	defer unsubscribe()
	require.NoError(t, client.Start(ctx))
	defer func() { require.NoError(t, client.Stop(context.Background())) }()
	waitEvent := func(kind river.EventKind) {
		t.Helper()
		for {
			select {
			case e := <-events:
				if e.Job.ID == jobID && e.Kind == kind {
					return
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	waitEvent(river.EventKindJobSnoozed)
	require.Equal(t, int32(1), providerWrites.Load())
	require.Equal(t, intents.StatusInFlight, m.statusOf(t, row.ID))
	// Explicitly model a dead executor's expired heartbeat, not a slow live call.
	m.exec(t, "UPDATE billing.rail_intents SET claimed_until=now()-interval '1 second' WHERE id=$1", row.ID)
	_, err = client.JobRetry(ctx, jobID)
	require.NoError(t, err)
	waitEvent(river.EventKindJobCompleted)
	require.Equal(t, int32(1), providerWrites.Load())
	require.Equal(t, int32(1), handler.reads.Load())
	require.Equal(t, intents.StatusSucceeded, m.statusOf(t, row.ID))
	// A lost completion acknowledgement causes a duplicate wakeup, not a write.
	duplicate, err := client.Insert(ctx, intents.OperationArgs{MerchantID: m.id, IntentID: row.ID}, &river.InsertOpts{Queue: QueueBilling})
	require.NoError(t, err)
	jobID = duplicate.Job.ID
	waitEvent(river.EventKindJobCompleted)
	require.Equal(t, int32(1), handler.writes.Load())
}

func TestOperationWakeDoesNotLoseEarlierScheduleOrCrossMerchant(t *testing.T) {
	ctx := t.Context()
	m := seedIntentMerchant(t)
	other := seedIntentMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	mctx := merchant.WithID(ctx, merchant.ID(m.id))
	store := intents.NewStore(d)
	row, err := store.Enqueue(mctx, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: "test_operation", IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	err = store.WakeOperation(merchant.WithID(ctx, merchant.ID(other.id)), row.ID, time.Now())
	require.ErrorIs(t, err, pgx.ErrNoRows)
	// Pending work is not accelerated by a notification.
	require.NoError(t, store.WakeOperation(mctx, row.ID, time.Now()))
	unchanged, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, row.NextAttemptAt, unchanged.NextAttemptAt)
	_, claimed, err := store.ClaimByID(mctx, row.ID, time.Now(), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	handler := &recordingIntentHandler{intentType: "test_operation"}
	worker := ProviderOperationWorker{DB: d, Registry: intents.NewRegistry(handler)}
	wakeErr := worker.Work(ctx, &river.Job[intents.OperationArgs]{Args: intents.OperationArgs{MerchantID: m.id, IntentID: row.ID}})
	var snooze *river.JobSnoozeError
	require.ErrorAs(t, wakeErr, &snooze)
	require.LessOrEqual(t, snooze.Duration, time.Minute, "in-flight recovery follows lease expiry, not the original hour-long admission delay")
	require.Zero(t, handler.executed)
	require.Zero(t, handler.verified)
	require.NoError(t, store.MarkUnknown(mctx, row.ID, time.Now().Add(time.Hour), "waiting for provider truth", nil))
	before, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.True(t, before.NextAttemptAt.After(time.Now()))
	require.NoError(t, store.WakeOperation(mctx, row.ID, time.Now()))
	after, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, after.Status)
	require.False(t, after.NextAttemptAt.After(time.Now()))
	var total, ready int
	require.NoError(t, d.Pool().QueryRow(ctx, "SELECT count(*),count(*) FILTER (WHERE scheduled_at<=now()) FROM "+pgx.Identifier{config.RiverSchema, "river_job"}.Sanitize()+" WHERE args->>'intent_id'=$1", row.ID.String()).Scan(&total, &ready))
	require.Equal(t, 2, total)
	require.Equal(t, 1, ready, "earlier notification is not deduplicated into the sleeping job")
	leased, ok, err := store.ClaimUnknownByID(mctx, row.ID, time.Now(), time.Now().Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.WakeOperation(mctx, row.ID, time.Now()))
	stillLeased, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, leased.ClaimedUntil, stillLeased.ClaimedUntil, "a webhook cannot steal the verifier's lease")
}

type pausedCompletionWorker struct {
	ProviderOperationWorker
	observed chan struct{}
	release  chan struct{}
	paused   atomic.Bool
}

func (w *pausedCompletionWorker) Work(ctx context.Context, job *river.Job[intents.OperationArgs]) error {
	err := w.ProviderOperationWorker.Work(ctx, job)
	if err == nil && w.paused.CompareAndSwap(false, true) {
		close(w.observed)
		select {
		case <-w.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func TestOperationRevivalWhilePriorRiverJobCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m := seedIntentMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	id := seedDueIntent(t, m, "test_revivable_operation")
	m.exec(t, "UPDATE billing.rail_intents SET status='superseded' WHERE id=$1", id)
	var key string
	require.NoError(t, m.pool.QueryRow(ctx, "SELECT idempotency_key FROM billing.rail_intents WHERE id=$1", id).Scan(&key))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	handler := &recordingIntentHandler{intentType: "test_revivable_operation"}
	worker := &pausedCompletionWorker{ProviderOperationWorker: ProviderOperationWorker{DB: d, Registry: intents.NewRegistry(handler), Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}}, observed: make(chan struct{}), release: make(chan struct{})}
	workers := river.NewWorkers()
	river.AddWorker(workers, worker)
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 1}}})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
	_, err = client.Insert(ctx, intents.OperationArgs{MerchantID: m.id, IntentID: id}, &river.InsertOpts{Queue: QueueBilling, UniqueOpts: river.UniqueOpts{ByArgs: true, ByQueue: true, ByState: []rivertype.JobState{rivertype.JobStateAvailable, rivertype.JobStatePending, rivertype.JobStateRunning, rivertype.JobStateRetryable, rivertype.JobStateScheduled}}})
	require.NoError(t, err)
	events, unsubscribe := client.Subscribe(river.EventKindJobCompleted)
	defer unsubscribe()
	require.NoError(t, client.Start(ctx))
	defer func() { require.NoError(t, client.Stop(context.Background())) }()
	select {
	case <-worker.observed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Existing generic deferred operations may explicitly revive a superseded
	// row. Financial paid/declined operation types cannot use this path.
	row, err := intents.NewStore(d).Enqueue(merchant.WithID(ctx, merchant.ID(m.id)), intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: handler.Type(), IdempotencyKey: key, Origin: intents.OriginSystem, NextAttemptAt: time.Now()})
	require.NoError(t, err)
	require.Equal(t, id, row.ID)
	require.Equal(t, intents.StatusPending, row.Status)
	var count int
	require.NoError(t, d.Pool().QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{config.RiverSchema, "river_job"}.Sanitize()+" WHERE args->>'intent_id'=$1", id.String()).Scan(&count))
	require.Equal(t, 2, count)
	close(worker.release)
	for completed := 0; completed < 2; {
		select {
		case e := <-events:
			var args intents.OperationArgs
			require.NoError(t, json.Unmarshal(e.Job.EncodedArgs, &args))
			if args.IntentID == id {
				completed++
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	require.Equal(t, intents.StatusSucceeded, m.statusOf(t, id))
	require.Equal(t, 1, handler.executed)
}

func TestOperationWakeUsesRuntimeClock(t *testing.T) {
	ctx := t.Context()
	m := seedIntentMerchant(t)
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	mctx := merchant.WithID(ctx, merchant.ID(m.id))
	store := intents.NewStore(d)
	now := time.Date(2025, 1, 2, 12, 0, 0, 0, time.UTC)
	row, err := store.Enqueue(mctx, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: "test_operation", IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: now})
	require.NoError(t, err)
	_, ok, err := store.ClaimByID(mctx, row.ID, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, store.MarkUnknown(mctx, row.ID, now.Add(time.Hour), "awaiting provider", nil))
	require.NoError(t, store.WakeOperation(mctx, row.ID, now))
	after, err := store.Get(mctx, row.ID)
	require.NoError(t, err)
	require.Equal(t, now, after.NextAttemptAt.UTC(), "notification must wake on the runtime clock, not the machine clock")
}
