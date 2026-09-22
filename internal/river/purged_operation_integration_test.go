//go:build integration

package riverjobs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

type purgeWakePermission struct{}

func (purgeWakePermission) AllowDestructive(context.Context, uuid.UUID) (bool, string) {
	return true, "isolated confirmed purge test"
}

type purgeWakeHandler struct{ url string }

func (*purgeWakeHandler) Type() string { return "test_terminal_purge_wake" }
func (*purgeWakeHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}
func (h *purgeWakeHandler) Execute(ctx context.Context, _ gen.OpenrailsRailIntent) intents.Outcome {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	resp.Body.Close()
	return intents.Succeeded(nil)
}
func (h *purgeWakeHandler) Verify(ctx context.Context, _ gen.OpenrailsRailIntent) intents.Outcome {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return intents.Ambiguous(err.Error())
	}
	resp.Body.Close()
	return intents.Succeeded(nil)
}
func (*purgeWakeHandler) Backoff(int32) time.Duration { return time.Minute }

func TestPurgedTerminalOperationWakeCompletesWithoutProviderCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	m := seedIntentMerchant(t)
	t.Cleanup(func() {
		_, err := m.pool.Exec(context.Background(), `DELETE FROM public.river_job WHERE args->>'merchant_id'=$1`, m.id.String())
		require.NoError(t, err)
	})
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	var providerCalls atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { providerCalls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer provider.Close()
	h := &purgeWakeHandler{url: provider.URL}
	registry := intents.NewRegistry(h)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	workers := river.NewWorkers()
	river.AddWorker(workers, &ProviderOperationWorker{DB: d, Registry: registry, Config: cfg})
	client, err := river.NewClient(riverpgxv5.New(d.Pool()), &river.Config{Schema: config.RiverSchema, Workers: workers, Queues: map[string]river.QueueConfig{QueueBilling: {MaxWorkers: 1}}, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond})
	require.NoError(t, err)
	d.SetRiverJobInserter(client)
	scoped := merchant.WithID(ctx, merchant.ID(m.id))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	store := intents.NewStore(d)
	row, err := store.Enqueue(scoped, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: h.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: time.Now()})
	require.NoError(t, err)
	runner := &intents.Runner{Store: store, Registry: registry, Config: cfg}
	row, err = runner.ExecuteByID(scoped, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, row.Status)
	require.EqualValues(t, 1, providerCalls.Load())
	var live int
	require.NoError(t, m.pool.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE merchant_id=$1 AND status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')`, m.id).Scan(&live))
	require.Zero(t, live, "purge fixture has only terminal operation history")
	var jobID int64
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT id FROM public.river_job WHERE args->>'intent_id'=$1`, row.ID.String()).Scan(&jobID))
	service, err := merchants.NewService(d.DataPool(), nil, "test")
	require.NoError(t, err)
	service.WithDestructivePolicy(purgeWakePermission{})
	inventory, err := service.TakePurgeInventory(ctx, merchant.ID(m.id))
	require.NoError(t, err)
	var slug string
	require.NoError(t, m.pool.QueryRow(ctx, `SELECT slug FROM billing.merchants WHERE id=$1`, m.id).Scan(&slug))
	require.NoError(t, service.Delete(ctx, merchant.ID(m.id), merchants.DeleteOptions{ConfirmPhrase: merchants.PurgeConfirmPhrase(slug), ExpectRows: &inventory.TotalRows, InventoryID: inventory.ID, Actor: "quiescent-purge-review"}))
	_, err = store.Get(scoped, row.ID)
	require.ErrorIs(t, err, pgx.ErrNoRows, "authorized purge removed the canonical operation")
	var state string
	require.NoError(t, m.pool.QueryRow(ctx, `SELECT status FROM billing.merchants WHERE id=$1`, m.id).Scan(&state))
	require.Equal(t, "deleted", state)
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT state FROM public.river_job WHERE id=$1`, jobID).Scan(&state))
	t.Logf("authorized terminal-only purge retained old River job %d in state %s", jobID, state)
	events, unsubscribe := client.Subscribe(river.EventKindJobSnoozed, river.EventKindJobCompleted)
	defer unsubscribe()
	require.NoError(t, client.Start(ctx))
	defer func() { cancel(); require.NoError(t, client.Stop(context.Background())) }()
	wait := func() river.EventKind {
		for {
			select {
			case event := <-events:
				if event.Job.ID == jobID {
					return event.Kind
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	outcome := wait()
	if outcome == river.EventKindJobSnoozed {
		for attempt := 0; attempt < 2; attempt++ {
			_, err = client.JobRetry(ctx, jobID)
			require.NoError(t, err)
			outcome = wait()
			require.Equal(t, river.EventKindJobSnoozed, outcome)
		}
		t.Log("definitively missing operation snoozed on all three attempts")
	}
	require.EqualValues(t, 1, providerCalls.Load(), "old purged wake cannot call a provider")
	require.Equal(t, river.EventKindJobCompleted, outcome, "a definitive missing ledger must not snooze forever")
}

func TestOperationLookupDatabaseFailureRetainsLiveWork(t *testing.T) {
	ctx := t.Context()
	m := seedIntentMerchant(t)
	t.Cleanup(func() {
		_, err := m.pool.Exec(context.Background(), `DELETE FROM public.river_job WHERE args->>'merchant_id'=$1`, m.id.String())
		require.NoError(t, err)
	})
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	var reads, writes atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			writes.Add(1)
		} else {
			reads.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer provider.Close()
	h := &purgeWakeHandler{url: provider.URL}
	scoped := merchant.WithID(ctx, merchant.ID(m.id))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	store := intents.NewStore(d)
	now := time.Now().UTC()
	row, err := store.Enqueue(scoped, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: h.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: now})
	require.NoError(t, err)
	_, claimed, err := store.ClaimByID(scoped, row.ID, now, now.Add(-time.Second))
	require.NoError(t, err)
	require.True(t, claimed)
	before, err := store.Get(scoped, row.ID)
	require.NoError(t, err)
	owner := dbtest.SharedSuperuserPGXPool(t)
	name := "fail_operation_lookup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	function := pgx.Identifier{"billing", name}.Sanitize()
	trigger := pgx.Identifier{name}.Sanitize()
	_, err = owner.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='%s'::uuid THEN RAISE EXCEPTION 'injected transient lookup failure' USING ERRCODE='40001'; END IF; RETURN NEW; END $$; CREATE TRIGGER %s BEFORE UPDATE ON billing.rail_intents FOR EACH ROW EXECUTE FUNCTION %s()`, function, row.ID, trigger, function))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := owner.Exec(context.Background(), "DROP FUNCTION IF EXISTS "+function+"() CASCADE")
		require.NoError(t, err)
	})
	worker := ProviderOperationWorker{DB: d, Registry: intents.NewRegistry(h), Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}}
	job := &river.Job[intents.OperationArgs]{Args: intents.OperationArgs{MerchantID: m.id, IntentID: row.ID}}
	err = worker.Work(ctx, job)
	var snooze *river.JobSnoozeError
	require.ErrorAs(t, err, &snooze, "a transient database error must retain the wake")
	after, err := store.Get(scoped, row.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Zero(t, reads.Load())
	require.Zero(t, writes.Load())
	var jobs int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE args->>'intent_id'=$1 AND state NOT IN ('completed','cancelled','discarded')`, row.ID.String()).Scan(&jobs))
	require.Equal(t, 1, jobs)
	_, err = owner.Exec(ctx, "DROP FUNCTION "+function+"() CASCADE")
	require.NoError(t, err)
	require.NoError(t, worker.Work(ctx, job))
	current, err := store.Get(scoped, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, current.Status)
	require.EqualValues(t, 1, reads.Load(), "recovered lookup verifies the accepted operation")
	require.Zero(t, writes.Load(), "database recovery must not re-execute an uncertain operation")
}

type missingRelatedRowHandler struct {
	lookup     func(context.Context, gen.OpenrailsRailIntent) error
	executions atomic.Int32
}

func (*missingRelatedRowHandler) Type() string { return "test_missing_related_row" }
func (h *missingRelatedRowHandler) CheckRelevance(ctx context.Context, in gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), h.lookup(ctx, in)
}
func (h *missingRelatedRowHandler) Execute(context.Context, gen.OpenrailsRailIntent) intents.Outcome {
	h.executions.Add(1)
	return intents.Succeeded(nil)
}
func (*missingRelatedRowHandler) Verify(context.Context, gen.OpenrailsRailIntent) intents.Outcome {
	return intents.Ambiguous("related data unresolved")
}
func (*missingRelatedRowHandler) Backoff(int32) time.Duration { return time.Minute }

func TestMissingRelatedRowDoesNotDiscardAcceptedOperation(t *testing.T) {
	ctx := t.Context()
	m := seedIntentMerchant(t)
	t.Cleanup(func() {
		_, err := m.pool.Exec(context.Background(), `DELETE FROM public.river_job WHERE args->>'merchant_id'=$1`, m.id.String())
		require.NoError(t, err)
	})
	d := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	related := uuid.New()
	h := &missingRelatedRowHandler{lookup: func(ctx context.Context, in gen.OpenrailsRailIntent) error {
		var id uuid.UUID
		return m.pool.QueryRow(ctx, `SELECT id FROM billing.customers WHERE merchant_id=$1 AND id=$2`, in.MerchantID, related).Scan(&id)
	}}
	scoped := merchant.WithID(ctx, merchant.ID(m.id))
	psp := dbtest.EnsureTestPSP(ctx, t, m.pool, m.id, "nmi")
	store := intents.NewStore(d)
	row, err := store.Enqueue(scoped, intents.EnqueueParams{MerchantID: m.id, Provider: "nmi", PspID: psp, IntentType: h.Type(), IdempotencyKey: uuid.NewString(), Origin: intents.OriginSystem, NextAttemptAt: time.Now()})
	require.NoError(t, err)
	registry := intents.NewRegistry(h)
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	worker := ProviderOperationWorker{DB: d, Registry: registry, Config: cfg}
	var beforeJobs int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE args->>'intent_id'=$1 AND state NOT IN ('completed','cancelled','discarded')`, row.ID.String()).Scan(&beforeJobs))
	workErr := worker.Work(ctx, &river.Job[intents.OperationArgs]{Args: intents.OperationArgs{MerchantID: m.id, IntentID: row.ID}})
	if workErr != nil {
		var snooze *river.JobSnoozeError
		require.ErrorAs(t, workErr, &snooze)
	}
	current, err := store.Get(scoped, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusPending, current.Status, "missing related data retains the accepted operation")
	require.Zero(t, h.executions.Load())
	var active int
	require.NoError(t, d.Pool().QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE args->>'intent_id'=$1 AND state NOT IN ('completed','cancelled','discarded')`, row.ID.String()).Scan(&active))
	require.Positive(t, active)
	if workErr == nil {
		require.Greater(t, active, beforeJobs, "retiring the consumed wake requires a durable successor")
	}
	_, err = m.pool.Exec(ctx, `INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2)`, related, m.id)
	require.NoError(t, err)
	runner := &intents.Runner{Store: store, Registry: registry, Config: cfg}
	current, err = runner.ExecuteByID(scoped, row.ID)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, current.Status)
	require.EqualValues(t, 1, h.executions.Load())
}
