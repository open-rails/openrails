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
	DSN, Schema, Slug, Gateway, Ready, ID, Mode string
	Now                                         time.Time
	PSPs                                        map[string]openrails.PSPConfig
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
	realClock := clockwork.NewRealClock()
	clock := restartClock{Clock: realClock, offset: input.Now.Sub(realClock.Now())}
	rt, err := openrails.New(t.Context(), openrails.Config{
		Schema: input.Schema, River: openrails.RiverHostOwned,
		TestMode: openrails.Sandbox, ProviderWriteMode: input.Mode,
		DB: &openrails.DBConfig{URL: input.DSN}, HTTP: &openrails.HTTPConfig{},
		Merchant: openrails.MerchantDeclaration{Slug: input.Slug, DisplayName: input.Slug, PSPs: input.PSPs},
	}, openrails.Deps{Postgres: pool, Clock: clock, NMITransport: restartNMITransport{gateway: gateway}})
	require.NoError(t, err)
	defer func() { _ = rt.Close(context.Background()) }()
	jobs, err := riverkit.New(t.Context(), pool, &river.Config{Schema: input.Schema, ID: input.ID, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 4}}}, rt.RiverJobs())
	require.NoError(t, err)
	require.NoError(t, jobs.Start(t.Context()))
	require.NoError(t, os.WriteFile(input.Ready, []byte("ready"), 0600))
	<-t.Context().Done()
}

// Historical business time continues advancing after boot, as production time
// does. A frozen clock would leave recovery backoffs permanently in the future.
type restartClock struct {
	clockwork.Clock
	offset time.Duration
}

func (c restartClock) Now() time.Time                   { return c.Clock.Now().Add(c.offset) }
func (c restartClock) Since(at time.Time) time.Duration { return c.Now().Sub(at) }
func (c restartClock) Until(at time.Time) time.Duration { return at.Sub(c.Now()) }

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

func startRestartWorker(t *testing.T, w *world, gateway, mode string) *restartWorkerProcess {
	t.Helper()
	dir := t.TempDir()
	input := restartWorkerConfig{DSN: w.dsn, Schema: w.schema, Slug: w.slug, Gateway: gateway, Ready: filepath.Join(dir, "ready"), ID: "restart-" + uuid.NewString(), Now: w.clock.Now(), PSPs: w.psps, Mode: mode}
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

// ageRestartQueue models the elapsed wall time of an offline fleet separately
// from its injected business clock. Job states, arguments, operation leases,
// receipts and provider coverage are unchanged; ordinary startup must resume
// or rescue the now-old queue work itself.
func ageRestartQueue(t *testing.T, w *world, offline time.Duration) {
	t.Helper()
	require.Positive(t, offline)
	rows, err := w.pool.Query(t.Context(), w.q(`SELECT id,kind,state,attempted_at,scheduled_at FROM billing.river_job WHERE state IN ('running','available','scheduled','retryable') ORDER BY id`))
	require.NoError(t, err)
	for rows.Next() {
		var id int64
		var kind, state string
		var attempted *time.Time
		var scheduled time.Time
		require.NoError(t, rows.Scan(&id, &kind, &state, &attempted, &scheduled))
		t.Logf("before %s outage aging: job=%d kind=%s state=%s attempted_at=%v scheduled_at=%s", offline, id, kind, state, attempted, scheduled)
	}
	require.NoError(t, rows.Err())
	rows.Close()
	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.river_job SET attempted_at=attempted_at-make_interval(secs => $1),scheduled_at=scheduled_at-make_interval(secs => $1) WHERE state IN ('running','available','scheduled','retryable')`), offline.Seconds())
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.river_leader SET expires_at=expires_at-make_interval(secs => $1)`), offline.Seconds())
	require.NoError(t, err)
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
				worker := startRestartWorker(t, w, gateway.URL, openrails.ProviderWritesFull)
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
			ageRestartQueue(t, w, 5*day)
			if stage == "not_admitted" {
				require.Equal(t, 0, restartCollectionCount(t, w, e, ""))
			}
			first := startRestartWorker(t, w, gateway.URL, openrails.ProviderWritesFull)
			second := startRestartWorker(t, w, gateway.URL, openrails.ProviderWritesFull)
			require.Eventually(t, func() bool { return restartCollectionCount(t, w, e, "succeeded") == 1 }, 90*time.Second, 50*time.Millisecond, "normal startup/due/rescue jobs finish the obligation without manual retry")
			require.Len(t, w.nmi.Ledger(""), 2, "one initial payment and exactly one renewal")
			require.Len(t, w.nmi.Attempts(), 2, "recovery does not submit an additional charge")
			if stage == "paid_before_completion" {
				require.Eventually(t, func() bool {
					var rescued int
					err := w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.provider_operation' AND metadata ? 'openrails:rescue_count'`)).Scan(&rescued)
					return err == nil && rescued > 0
				}, 30*time.Second, 50*time.Millisecond, "normal startup rescue independently reclaims the killed job after financial recovery")
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

// Monthly-floor collection must survive restarts without waiting for a new
// thirty-day in-memory timer, and must retain its durable monthly cadence.
func TestNMIInvoiceFiveDayStartupRecovery(t *testing.T) {
	for _, mode := range []string{openrails.ProviderWritesFull, openrails.ProviderWritesReadOnly} {
		t.Run(mode, func(t *testing.T) {
			f := startFleet(t, 1, false, nil, func(w *world) {
				w.declare = func(psps map[string]openrails.PSPConfig) { delete(psps, "stripe"); delete(psps, "ccbill") }
			})
			w := f.any()
			// Initial provider observation legitimately completes this month's
			// empty collection scan. The five-day outage must cross the next
			// monthly boundary before the later small invoice is eligible.
			setup := w.clock.Now().UTC().Truncate(30 * day).Add(28 * day)
			if !setup.After(w.clock.Now()) {
				setup = setup.Add(30 * day)
			}
			f.advance(setup.Sub(w.clock.Now()))
			c := w.newCustomer()
			method := c.saveCard("nmi", visa)
			initial := newNMIInvoice(f, c)
			w.pull() // Qualify the setup invoice's historical period through the real provider refresh.
			w.settleCollectionScans()
			answer := payNMIInvoice(t.Context(), w, c, initial, method, "invoice-startup-agreement")
			require.NoError(t, answer.err)
			require.Contains(t, []int{http.StatusOK, http.StatusAccepted}, answer.status)
			f.settle()
			requireInvoicePaidOnce(f, initial, 1, 1, nmiInvoiceAmount, 0)
			c.must(http.MethodPut, "/collection-payment-method", "", map[string]any{"currency": "USD", "payment_method_id": method})
			smallInvoice := func() billing.InvoiceID {
				id := newNMIInvoice(f, c)
				_, err := w.client[remote].CreateInvoicePayment(t.Context(), id, billing.CreateInvoicePaymentParams{Amount: 90_000_000, Reference: "partial-" + id.String()})
				require.NoError(t, err)
				return id
			}
			invoice := smallInvoice() // $10: below the $50 hourly threshold, above the $1 monthly floor.
			var previousPeriod time.Time
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT monthly_period_started_at FROM billing.invoice_collection_cadence`)).Scan(&previousPeriod))
			require.True(t, previousPeriod.Equal(w.clock.Now().UTC().Truncate(30*day)), "healthy setup already completed this month's collection scan")
			w.stop()
			gateway := httptest.NewServer(w.nmi)
			t.Cleanup(gateway.Close)
			if mode == openrails.ProviderWritesReadOnly {
				observer := startRestartWorker(t, w, gateway.URL, mode)
				require.Eventually(t, func() bool {
					var pending int
					err := w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.invoice' AND args->>'use_monthly_floor'='true' AND state='scheduled'`)).Scan(&pending)
					return err == nil && pending == 1
				}, 30*time.Second, 50*time.Millisecond, "a disabled collection pass stays pending instead of consuming its monthly slot")
				observer.kill(t)
				require.Len(t, w.nmi.Attempts(), 1)
			}
			f.advance(5 * day)
			require.True(t, w.clock.Now().UTC().Truncate(30*day).After(previousPeriod), "the outage crosses the next monthly collection boundary")
			ageRestartQueue(t, w, 5*day)
			first := startRestartWorker(t, w, gateway.URL, openrails.ProviderWritesFull)
			second := startRestartWorker(t, w, gateway.URL, openrails.ProviderWritesFull)
			require.Eventually(t, func() bool {
				var status string
				err := w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.invoices WHERE id=$1`), invoice.UUID()).Scan(&status)
				return err == nil && status == "paid"
			}, 30*time.Second, 50*time.Millisecond, "ordinary startup recovers monthly-floor invoice collection")
			require.Eventually(t, func() bool {
				var completed int
				err := w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.invoice' AND args->>'use_monthly_floor'='true' AND state='completed'`)).Scan(&completed)
				var period time.Time
				cadenceErr := w.pool.QueryRow(t.Context(), w.q(`SELECT monthly_period_started_at FROM billing.invoice_collection_cadence`)).Scan(&period)
				return err == nil && completed > 0 && cadenceErr == nil && period.Equal(w.clock.Now().UTC().Truncate(30*day))
			}, 15*time.Second, 50*time.Millisecond, "the complete monthly scan retains its cadence before shutdown")
			first.kill(t)
			second.kill(t)
			w.start()
			requireInvoicePaidOnce(f, invoice, 2, 2, 10_000_000, 1)
			next := smallInvoice()
			w.stop()
			// River normally prunes completed jobs after a day. Remove only the
			// completed monthly job: billing cadence must outlive this queue GC.
			_, err := w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.river_job WHERE kind='openrails.invoice' AND args->>'use_monthly_floor'='true' AND state='completed'`))
			require.NoError(t, err)
			var completed int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.invoice' AND args->>'use_monthly_floor'='true' AND state='completed'`)).Scan(&completed))
			require.Zero(t, completed, "GC removed every completed monthly scan")
			var retainedPeriod, retainedCompletion time.Time
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT monthly_period_started_at, completed_at FROM billing.invoice_collection_cadence`)).Scan(&retainedPeriod, &retainedCompletion))
			_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.river_leader SET expires_at=now()-interval '1 minute'`))
			require.NoError(t, err)
			third := startRestartWorker(t, w, gateway.URL, openrails.ProviderWritesFull)
			// Startup may reuse the original monthly job, still waiting on its
			// one-minute readonly snooze or recovery retry. Its ID need not be
			// new. With old completions gone, any completed scan proves that the
			// restarted worker checked the durable cadence after queue GC.
			require.Eventually(t, func() bool {
				var passes int
				err := w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.river_job WHERE kind='openrails.invoice' AND args->>'use_monthly_floor'='true' AND state='completed'`)).Scan(&passes)
				return err == nil && passes > 0
			}, 90*time.Second, 50*time.Millisecond, "a post-GC monthly scan has checked retained cadence")
			third.kill(t)
			var checkedPeriod, checkedCompletion time.Time
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT monthly_period_started_at, completed_at FROM billing.invoice_collection_cadence`)).Scan(&checkedPeriod, &checkedCompletion))
			require.True(t, retainedPeriod.Equal(checkedPeriod) && retainedCompletion.Equal(checkedCompletion), "post-GC scanning preserves the completed billing cadence")
			require.Len(t, w.nmi.Attempts(), 2, "a new small balance waits for the next monthly pass")
			w.start()
			unpaid, err := w.client[remote].GetInvoice(t.Context(), next)
			require.NoError(t, err)
			require.Equal(t, int64(10_000_000), unpaid.AmountDue)
			w.stop()
			f.advance(32 * day)
			ageRestartQueue(t, w, 32*day)
			_, err = w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.river_job WHERE kind='openrails.invoice' AND state='completed'`))
			require.NoError(t, err)
			fourth := startRestartWorker(t, w, gateway.URL, openrails.ProviderWritesFull)
			require.Eventually(t, func() bool {
				var status string
				err := w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.invoices WHERE id=$1`), next.UUID()).Scan(&status)
				return err == nil && status == "paid"
			}, 30*time.Second, 50*time.Millisecond, "the next monthly period recovers the remaining small invoice")
			fourth.kill(t)
			w.start()
			requireInvoicePaidOnce(f, next, 3, 3, 10_000_000, 1)
			var period time.Time
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT monthly_period_started_at FROM billing.invoice_collection_cadence`)).Scan(&period))
			require.True(t, period.Equal(w.clock.Now().UTC().Truncate(30*day)), "cadence advanced only after the complete scan")
		})
	}
}
