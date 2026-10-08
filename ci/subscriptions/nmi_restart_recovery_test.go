//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

const restartWorkerEnv = "OPENRAILS_NMI_RESTART_WORKER"

type restartWorkerConfig struct {
	DSN, Schema, Slug, Gateway, Ready, ID string
	Now                                   time.Time
	PSPs                                  map[string]openrails.PSPConfig
}

// TestNMIRecoveryWorkerProcess is a real independent host process. It mounts the
// normal RiverJobs and leaves their startup schedules, rescue and polling intact.
// Only business time and the provider network boundary are test-controlled.
func TestNMIRecoveryWorkerProcess(t *testing.T) {
	raw := os.Getenv(restartWorkerEnv)
	if raw == "" {
		t.Skip("child process entry point")
	}
	var input restartWorkerConfig
	require.NoError(t, json.Unmarshal([]byte(raw), &input))
	pool, err := pgxpool.New(t.Context(), input.DSN)
	require.NoError(t, err)
	defer pool.Close()
	gateway, err := url.Parse(input.Gateway)
	require.NoError(t, err)
	rt, err := openrails.New(t.Context(), openrails.Config{
		Schema: input.Schema, River: openrails.RiverHostOwned,
		TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
		DB: &openrails.DBConfig{URL: input.DSN}, HTTP: &openrails.HTTPConfig{},
		Merchant: openrails.MerchantDeclaration{Slug: input.Slug, DisplayName: input.Slug, PSPs: input.PSPs},
	}, openrails.Deps{Postgres: pool, Clock: clockwork.NewFakeClockAt(input.Now), NMITransport: restartNMITransport{gateway: gateway}})
	require.NoError(t, err)
	defer func() { _ = rt.Close(context.Background()) }()
	jobs, err := riverkit.New(t.Context(), pool, &river.Config{Schema: input.Schema, ID: input.ID, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 4}}}, rt.RiverJobs())
	require.NoError(t, err)
	require.NoError(t, jobs.Start(t.Context()))
	require.NoError(t, os.WriteFile(input.Ready, []byte("ready"), 0600))
	<-t.Context().Done()
}

type restartNMITransport struct{ gateway *url.URL }

func (t restartNMITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	request.URL.Scheme, request.URL.Host = t.gateway.Scheme, t.gateway.Host
	request.Host = t.gateway.Host
	return http.DefaultTransport.RoundTrip(request)
}

type restartWorkerProcess struct {
	command *exec.Cmd
	done    chan error
	once    sync.Once
}

func startRestartWorker(t *testing.T, w *world, gateway string) *restartWorkerProcess {
	t.Helper()
	dir := t.TempDir()
	input := restartWorkerConfig{DSN: w.dsn, Schema: w.schema, Slug: w.slug, Gateway: gateway, Ready: filepath.Join(dir, "ready"), ID: "restart-" + uuid.NewString(), Now: w.clock.Now(), PSPs: w.psps}
	raw, err := json.Marshal(input)
	require.NoError(t, err)
	logPath := filepath.Join(dir, "worker.log")
	log, err := os.Create(logPath)
	require.NoError(t, err)
	command := exec.Command(os.Args[0], "-test.run=^TestNMIRecoveryWorkerProcess$", "-test.timeout=3m")
	command.Env = append(os.Environ(), restartWorkerEnv+"="+string(raw), "GOMAXPROCS=2")
	command.Stdout, command.Stderr = log, log
	require.NoError(t, command.Start())
	worker := &restartWorkerProcess{command: command, done: make(chan error, 1)}
	go func() { worker.done <- command.Wait(); close(worker.done); _ = log.Close() }()
	t.Cleanup(func() {
		worker.kill(t)
		if t.Failed() {
			raw, _ := os.ReadFile(logPath)
			t.Logf("child %s: %s", input.ID, raw)
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(input.Ready); return err == nil }, 20*time.Second, 20*time.Millisecond, "independent worker startup; log %s", logPath)
	return worker
}
func (w *restartWorkerProcess) kill(t *testing.T) {
	t.Helper()
	w.once.Do(func() {
		_ = w.command.Process.Kill()
		select {
		case <-w.done:
		case <-time.After(10 * time.Second):
			t.Fatal("worker process did not stop")
		}
	})
}

// Whole-period catch-up is a different policy. These outages last five days
// inside one monthly obligation, with no worker alive during the downtime.
func TestNMIFiveDayRestartRecovery(t *testing.T) {
	for _, stage := range []string{"not_admitted", "queued", "paid_before_completion"} {
		t.Run(stage, func(t *testing.T) {
			w := prepareWorld(t, 12)
			w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
			w.start()
			e := enroll(t, w, "nmi", embedded)
			end := e.periodEnd()
			w.settle()
			gateway := httptest.NewServer(w.nmi)
			t.Cleanup(gateway.Close)
			if stage == "queued" {
				// A host's admission queue can run while payment executors are down.
				// Let the real due worker commit its operation/job, with no billing
				// consumer alive. No operation or financial row is manufactured.
				jobs := w.jobs
				jobs.PeriodicJobs().Clear()
				require.NoError(t, jobs.Queues().Remove(t.Context(), openrails.QueueBilling))
				require.NoError(t, jobs.Queues().Add("admission_only", river.QueueConfig{MaxWorkers: 1}))
				e.toPeriodEnd()
				pass, err := jobs.Insert(t.Context(), dunningPass{}, &river.InsertOpts{Queue: "admission_only"})
				require.NoError(t, err)
				require.Eventually(t, func() bool {
					j, err := jobs.JobGet(t.Context(), pass.Job.ID)
					return err == nil && j.State == rivertype.JobStateCompleted
				}, 15*time.Second, 20*time.Millisecond, "due admission committed")
				require.Equal(t, 1, restartCollectionCount(t, w, e, "pending"))
				var unfenced int
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='subscription_collection' AND NOT coalesce(result_evidence,'{}'::jsonb) ? 'submitted_at'`), subUUID(e.sub)).Scan(&unfenced))
				require.Equal(t, 1, unfenced)
				require.Len(t, w.nmi.Ledger(""), 1, "queued charge has not reached the provider")
			}
			w.stop()
			if stage == "paid_before_completion" {
				w.advance(end.Sub(w.clock.Now()) + time.Second)
				// The gateway has paid; the executor persisted its candidate and is
				// blocked reading that receipt when the OS kills its process.
				gate := w.nmi.hold(newGate(func(r *http.Request) bool {
					return (strings.HasSuffix(r.URL.Path, "/query.php") || r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v5/")) && len(w.nmi.Ledger("")) >= 2
				}, false))
				t.Cleanup(func() {
					w.nmi.unhold()
					select {
					case <-gate.release:
					default:
						close(gate.release)
					}
				})
				worker := startRestartWorker(t, w, gateway.URL)
				select {
				case <-gate.arrived:
				case <-time.After(30 * time.Second):
					t.Fatal("paid receipt read did not arrive")
				}
				worker.kill(t)
				w.nmi.unhold()
				close(gate.release)
				var candidate bool
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT result_evidence ? 'collection_candidate' FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='subscription_collection'`), subUUID(e.sub)).Scan(&candidate))
				require.True(t, candidate, "accepted provider receipt survives process death")
				require.Equal(t, 1, restartCollectionCount(t, w, e, "in_flight"))
				require.Len(t, w.nmi.Ledger(""), 2, "renewal already paid before local completion")
			}
			w.advance(end.Add(5 * 24 * time.Hour).Sub(w.clock.Now()))
			// River uses wall time for liveness; business time is injected separately.
			// Age only its scheduling/liveness evidence as the five-day outage would.
			// Accepted operation payloads, leases, receipts and billing facts stay intact.
			_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.river_job SET attempted_at=attempted_at-interval '5 days',scheduled_at=scheduled_at-interval '5 days' WHERE state IN ('running','available','scheduled','retryable')`))
			require.NoError(t, err)
			_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.river_leader SET expires_at=now()-interval '5 days'`))
			require.NoError(t, err)
			if stage == "not_admitted" {
				require.Equal(t, 0, restartCollectionCount(t, w, e, ""))
			}
			first := startRestartWorker(t, w, gateway.URL)
			second := startRestartWorker(t, w, gateway.URL)
			require.Eventually(t, func() bool { return restartCollectionCount(t, w, e, "succeeded") == 1 }, 90*time.Second, 50*time.Millisecond, "normal startup/due/rescue jobs finish the obligation without manual retry")
			require.Len(t, w.nmi.Ledger(""), 2, "one initial payment and exactly one renewal")
			require.Len(t, w.nmi.Attempts(), 2, "recovery does not submit an additional charge")
			if stage == "paid_before_completion" {
				var rescued int
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.provider_operation' AND metadata ? 'openrails:rescue_count'`)).Scan(&rescued))
				require.Greater(t, rescued, 0, "normal startup rescue reclaimed the killed job")
			}
			first.kill(t)
			second.kill(t)
			// Public reads through a fresh runtime confirm the recovered billing facts.
			w.start()
			require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
			require.True(t, end.Add(monthHours*time.Hour).Equal(e.periodEnd()), "one original billing period, not five days of shifted cadence")
			require.True(t, e.c.entitled(e.ent))
			paid := completed(w.payments(embedded, e.c.id))
			require.Len(t, paid, 2)
			e.requireLedgerAgreement(paid)
			require.Len(t, w.nmi.Ledger(""), 2)
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

func restartCollectionCount(t *testing.T, w *world, e *engineCase, status string) int {
	t.Helper()
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='subscription_collection' AND ($2='' OR status=$2)`), subUUID(e.sub), status).Scan(&count))
	return count
}
