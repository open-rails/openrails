//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

type hostChore struct{}

func (hostChore) Kind() string { return "critical.host_chore" }

type hostChoreWorker struct {
	river.WorkerDefaults[hostChore]
	ran chan int64
}

func (w *hostChoreWorker) Work(_ context.Context, job *river.Job[hostChore]) error {
	w.ran <- job.ID
	return nil
}

func criticalRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	url := os.Getenv("OPENRAILS_CRITICAL_REDIS_URL")
	require.NotEmpty(t, url, "OPENRAILS_CRITICAL_REDIS_URL must name a disposable Redis; critical scenarios never skip")
	options, err := redis.ParseURL(url)
	require.NoError(t, err)
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, client.Ping(t.Context()).Err())
	return client
}

// storageFault makes the world's own storage refuse one class of write: a
// fixture fault at the database boundary, never a substituted result.
func storageFault(t *testing.T, w *world, schema, table, refuse string) func() {
	t.Helper()
	name := "critical_fault_" + table
	fn := pgx.Identifier{w.schema, name}.Sanitize()
	_, err := w.pool.Exec(t.Context(), fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN IF %s THEN RAISE EXCEPTION 'critical storage fault'; END IF; RETURN NEW; END $$;
CREATE TRIGGER %s BEFORE INSERT ON %s FOR EACH ROW EXECUTE FUNCTION %s()`,
		fn, refuse, pgx.Identifier{name}.Sanitize(), pgx.Identifier{schema, table}.Sanitize(), fn))
	require.NoError(t, err)
	return func() {
		_, err := w.pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER %s ON %s", pgx.Identifier{name}.Sanitize(), pgx.Identifier{schema, table}.Sanitize()))
		require.NoError(t, err)
	}
}

// runRegisteredFails inserts one job and requires the registered worker to
// return its error to River.
func runRegisteredFails(t *testing.T, fleet *river.Client[pgx.Tx], kind string) {
	t.Helper()
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	job, err := fleet.Insert(t.Context(), registeredJob{KindName: kind}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event == nil || event.Job.ID != job.Job.ID {
				continue
			}
			require.Equal(t, river.EventKindJobFailed, event.Kind, "%s must report the merchant it could not finish", kind)
			return
		case <-deadline.C:
			t.Fatalf("registered %s job did not finish", kind)
		}
	}
}

func deniedAdmissions(t *testing.T, w *world, from time.Time) int64 {
	t.Helper()
	result, err := w.alpha.QueryMetrics(t.Context(), billing.MetricsQuery{Measures: []string{"admission_denials"},
		Range: &billing.MetricsRange{From: from.Format(time.RFC3339), To: from.Add(time.Hour).Format(time.RFC3339)}})
	require.NoError(t, err)
	var total int64
	for _, row := range result.Rows {
		var n int64
		_, err := fmt.Sscan(fmt.Sprint(row[len(row)-1]), &n)
		require.NoError(t, err)
		total += n
	}
	return total
}

func TestBackgroundJobs(t *testing.T) {
	t.Run("OR-J04-S07/host-worker-on-the-composed-fleet-is-rescued-after-a-crash", func(t *testing.T) {
		w := newWorld(t)
		chores := &hostChoreWorker{ran: make(chan int64, 4)}
		workers := river.NewWorkers()
		river.AddWorker(workers, chores)
		fleet, err := riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema, Workers: workers, RescueStuckJobsAfter: time.Minute,
			Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}, "host": {MaxWorkers: 1}}}, w.engine.RiverJobs())
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, fleet.StopAndCancel(ctx))
		})
		job, err := fleet.Insert(t.Context(), hostChore{}, &river.InsertOpts{Queue: "host"})
		require.NoError(t, err)
		// The host's process died while it ran the job.
		_, err = w.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{w.schema, "river_job"}.Sanitize()+
			" SET state='running', attempt=1, attempted_at=now()-interval '10 minutes', attempted_by=ARRAY['crashed-host'] WHERE id=$1", job.Job.ID)
		require.NoError(t, err)
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		select {
		case id := <-chores.ran:
			require.Equal(t, job.Job.ID, id)
		case <-time.After(60 * time.Second):
			t.Fatal("River's rescuer never returned the host's crashed job to work")
		}
	})
	t.Run("OR-J23-S06/denials-count-each-closed-hour-once", func(t *testing.T) {
		w := newWorld(t, func(w *world) { w.deps.Redis = criticalRedis(t) })
		customer := billing.CustomerID(uuid.New())
		hour := w.clock.Now().UTC().Truncate(time.Hour)
		for i := range 3 {
			results, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, customer, fmt.Sprintf("denied-%d", i), 500)})
			require.NoError(t, err)
			require.False(t, results[0].Allowed(), "an unfunded customer is denied")
		}
		fleet := startFleet(t, w)
		runRegistered(t, fleet, "openrails.admission_denial_flush", 1)
		require.Zero(t, deniedAdmissions(t, w, hour), "an open hour is not flushed")
		// A count whose customer no longer exists is dropped alone.
		key := fmt.Sprintf("or:mdeny:%s:%d", w.alpha.MerchantID().UUID(), hour.Unix())
		require.NoError(t, w.deps.Redis.HIncrBy(t.Context(), key, uuid.NewString()+"|insufficient_balance", 2).Err())
		w.clock.Advance(hour.Add(time.Hour + 10*time.Minute).Sub(w.clock.Now()))
		runRegistered(t, fleet, "openrails.admission_denial_flush", 2)
		require.EqualValues(t, 3, deniedAdmissions(t, w, hour))
		require.Zero(t, w.deps.Redis.Exists(t.Context(), key).Val(), "a flushed hour leaves Redis")
		runRegistered(t, fleet, "openrails.admission_denial_flush", 1)
		require.EqualValues(t, 3, deniedAdmissions(t, w, hour), "a flushed hour is counted once")
	})
	t.Run("OR-J29-S07/one-merchants-failed-pass-fails-the-job-after-the-others-finish", func(t *testing.T) {
		w := newWorld(t)
		customer := billing.CustomerID(uuid.New())
		lots := map[*openrails.Client]billing.CreditGrantID{}
		for _, client := range []*openrails.Client{w.alpha, w.beta} {
			grants, err := client.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "expiring", ExpiresAt: ptr(w.clock.Now().Add(time.Hour))}})
			require.NoError(t, err)
			lots[client] = grants[0].ID
		}
		restore := storageFault(t, w, w.schema, "ledger_transfers", fmt.Sprintf("NEW.merchant_id = '%s'::uuid", w.alpha.MerchantID().UUID()))
		w.clock.Advance(time.Hour)
		fleet := startFleet(t, w)
		runRegisteredFails(t, fleet, "openrails.balance_watch")
		beta, err := w.beta.GetCreditGrant(t.Context(), lots[w.beta])
		require.NoError(t, err)
		require.EqualValues(t, 1000, beta.ExpiredAmount, "the other merchant's pass finishes")
		alpha, err := w.alpha.GetCreditGrant(t.Context(), lots[w.alpha])
		require.NoError(t, err)
		require.Zero(t, alpha.ExpiredAmount)
		restore()
		runRegistered(t, fleet, "openrails.balance_watch", 1)
		alpha, err = w.alpha.GetCreditGrant(t.Context(), lots[w.alpha])
		require.NoError(t, err)
		require.EqualValues(t, 1000, alpha.ExpiredAmount)
	})
	t.Run("OR-J29-S08/cleanup-alone-ages-out-unpaid-orders-counters-and-orphan-state", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { w.deps.Redis = criticalRedis(t) })
		w := a.world
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, price := publicSale(t, a, "retained-order", 9_990_000, nil)
		unpaid, err := a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "closed-unpaid"})
		require.NoError(t, err)
		_, err = a.alphaRemote.CancelOrder(t.Context(), unpaid.ID)
		require.NoError(t, err)
		challenged := saleInput(t, a, p)
		p.core.SetDecline("4242", "auth")
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, challenged, map[string]string{"Idempotency-Key": "abandoned-challenge"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var waiting billing.Order
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.Equal(t, billing.OrderPaymentRequiresAction, waiting.Payment.Status)
		attempt := func() string {
			var state string
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT status FROM "+pgx.Identifier{w.schema, "order_attempts"}.Sanitize()+" WHERE order_id = $1", waiting.ID.UUID()).Scan(&state))
			return state
		}
		require.Contains(t, []string{"created", "requires_action"}, attempt())
		hour := w.clock.Now().UTC().Truncate(time.Hour)
		denied, err := w.alpha.Admit(t.Context(), []billing.AdmitParams{reservation(w, billing.CustomerID(uuid.New()), "denied", 500)})
		require.NoError(t, err)
		require.False(t, denied[0].Allowed())
		// The deployment-global state of a kind no fleet runs any more.
		orphans := func() int {
			var n int
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM "+pgx.Identifier{w.schema, "periodic_leases"}.Sanitize()+" WHERE kind = 'openrails.retired') + (SELECT count(*) FROM "+pgx.Identifier{w.schema, "worker_state"}.Sanitize()+" WHERE worker_kind = 'openrails.retired')").Scan(&n))
			return n
		}
		_, err = w.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{w.schema, "periodic_leases"}.Sanitize()+" (kind, holder, expires_at) VALUES ('openrails.retired', 'gone', now() - interval '31 days')")
		require.NoError(t, err)
		_, err = w.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{w.schema, "worker_state"}.Sanitize()+" (worker_kind, updated_at) VALUES ('openrails.retired', now() - interval '31 days')")
		require.NoError(t, err)
		require.Equal(t, 2, orphans())

		w.clock.Advance(25 * time.Hour)
		runRegistered(t, fleet, "openrails.admission_denial_flush", 1)
		require.EqualValues(t, 1, deniedAdmissions(t, w, hour))
		runRegistered(t, fleet, "openrails.cleanup_expired_data", 1)
		require.Contains(t, []string{"created", "requires_action"}, attempt(), "only the order expiry expires an attempt")
		require.Zero(t, orphans())
		runRegistered(t, fleet, "openrails.order_expiry", 1)
		require.Equal(t, "expired", attempt())

		w.clock.Advance(91 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.order_expiry", 1)
		_, err = a.alphaRemote.GetOrder(t.Context(), unpaid.ID)
		require.NoError(t, err, "the order expiry deletes nothing")
		runRegistered(t, fleet, "openrails.cleanup_expired_data", 1)
		_, err = a.alphaRemote.GetOrder(t.Context(), unpaid.ID)
		require.ErrorIs(t, err, billing.ErrNotFound)
		require.EqualValues(t, 1, deniedAdmissions(t, w, hour))
		w.clock.Advance(397 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.cleanup_expired_data", 1)
		require.Zero(t, deniedAdmissions(t, w, hour))
	})
	t.Run("OR-J29-S30/converge-sweep-fails-its-job-after-the-other-merchants-converge", func(t *testing.T) {
		w := newWorld(t, func(w *world) { w.cfg.ProviderWriteMode = openrails.ProviderWritesFull })
		customer := billing.CustomerID(uuid.New())
		for _, client := range []*openrails.Client{w.alpha, w.beta} {
			product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sweep-reader", DisplayName: "Sweep reader", Entitlements: []string{"sweep:reader"}})
			require.NoError(t, err)
			_, err = client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: "sweep-reader",
				Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, Hours: ptr(720), Reason: billing.GrantReasonComp}}})
			require.NoError(t, err)
		}
		// Both grants lost their projection: drift the sweep repairs.
		_, err := w.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{w.schema, "product_access"}.Sanitize()+" SET deleted_at=now()")
		require.NoError(t, err)
		reads := func(client *openrails.Client) bool {
			access, err := client.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sweep:reader"}})
			require.NoError(t, err)
			return access.Entitlements["sweep:reader"]
		}
		require.False(t, reads(w.alpha))
		restore := storageFault(t, w, w.schema, "product_access", fmt.Sprintf("NEW.merchant_id = '%s'::uuid", w.alpha.MerchantID().UUID()))
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegisteredFails(t, fleet, "openrails.converge_sweep")
		require.True(t, reads(w.beta), "the other merchant converges")
		require.False(t, reads(w.alpha))
		restore()
		runRegistered(t, fleet, "openrails.converge_sweep", 1)
		require.True(t, reads(w.alpha))
	})
	t.Run("OR-J37-S06/solana-renewal-waits-for-the-crank-before-a-missed-rebill", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		fleet := hostFleet(t, s.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, s.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		missed := func() int {
			findings, err := s.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.rebill.missed"})
			require.NoError(t, err)
			return len(findings.Items)
		}
		// Past the due pass's deadline, before the hourly crank's.
		s.clock.Advance(due.Add(90 * time.Minute).Sub(s.clock.Now()))
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		require.Zero(t, missed(), "a Solana renewal is pulled by the crank, not the due pass")
		s.clock.Advance(due.Add(3*time.Hour + time.Minute).Sub(s.clock.Now()))
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		require.Equal(t, 1, missed(), "a renewal no crank pulled is a missed rebill")
	})
	t.Run("OR-J21-S05", func(t *testing.T) {
		t.Run("attribution-no-pull-reads-becomes-a-finding", func(t *testing.T) {
			w := newWorld(t, func(w *world) { declareCCBill(w, nil) })
			customer := importCCBillAttribution(t, w)
			fleet := startFleet(t, w)
			runRegistered(t, fleet, "openrails.charge_attributions", 1)
			findings := unverifiedTransactions(t, w)
			require.Len(t, findings, 1, "a charge nothing reads is evidence for review, not a perpetual wait")
			require.Equal(t, "0112233446", findings[0].Evidence["transaction_id"])
			require.Len(t, ccbillPayments(t, w, customer), 1, "only the declared initial sale is a payment")
		})
		t.Run("attribution-the-pull-records-raises-no-finding", func(t *testing.T) {
			var feed *httptest.Server
			w := newWorld(t, func(w *world) {
				feed = dataLinkFeed(t, w.clock.Now().Truncate(time.Second).Add(-24*time.Hour))
				declareCCBill(w, feed)
			})
			customer := importCCBillAttribution(t, w)
			fleet := hostFleet(t, w)
			fleet.PeriodicJobs().Clear()
			require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
			require.NoError(t, fleet.Start(t.Context()))
			runRegistered(t, fleet, "openrails.charge_attributions", 1)
			require.Empty(t, unverifiedTransactions(t, w), "the DataLink pull has not read the charge yet")
			refreshMerchant(t, fleet, w.alpha.MerchantID().UUID())
			payments := ccbillPayments(t, w, customer)
			require.Contains(t, payments, "0112233446", "the pull records the attributed rebill")
			require.EqualValues(t, 10_000_000, payments["0112233446"])
			runRegistered(t, fleet, "openrails.charge_attributions", 1)
			require.Empty(t, unverifiedTransactions(t, w))
		})
	})
	t.Run("OR-J15-S30/rebill-watch-backs-off-a-failing-probe", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		var down atomic.Bool
		var probes atomic.Int32
		p.core.FailRequests(func(r *http.Request) bool {
			if down.Load() && strings.HasSuffix(r.URL.Path, "/query.php") {
				probes.Add(1)
				return true
			}
			return false
		}, http.StatusServiceUnavailable, 1_000_000)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + 25*time.Hour)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		down.Store(true)
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		failed := probes.Load()
		require.NotZero(t, failed)
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		require.Equal(t, failed, probes.Load(), "a failed probe waits out its backoff")
		down.Store(false)
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.rebill.missed"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1, "the probe runs again once its backoff has passed")
	})
	t.Run("OR-J06-S07/psp-import-commits-with-its-job", func(t *testing.T) {
		a, _ := newNMIWorld(t)
		// A bound producer that is never started: the pull job is queued, not run.
		fleet := hostFleet(t, a.world)
		// The PSP's onboarding import, never run here, holds its one import
		// in flight: set it aside so a manual import may start.
		require.Eventually(t, func() bool {
			tag, err := a.pool.Exec(t.Context(), fmt.Sprintf(`UPDATE %s SET status = 'failed', error = 'set aside by the test' WHERE origin = 'onboarding' AND status = 'pulling'`,
				pgx.Identifier{a.schema, "psp_imports"}.Sanitize()))
			require.NoError(t, err)
			return tag.RowsAffected() > 0
		}, 30*time.Second, 50*time.Millisecond)
		restore := storageFault(t, a.world, a.world.schema, "river_job", "NEW.kind = 'openrails.psp_import'")
		_, err := a.alpha.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		require.Error(t, err)
		// The PSP's onboarding import may hold the other psp_import job.
		imports, err := a.alpha.ListPSPImports(t.Context(), billing.PSPImportListParams{Origin: billing.PSPImportManual})
		require.NoError(t, err)
		require.Empty(t, imports.Items, "an import whose job was not queued is not kept")
		restore()
		created, err := a.alpha.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "nmi"}})
		require.NoError(t, err)
		require.Equal(t, billing.PSPImportPulling, created.Status)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import"))
		require.NoError(t, err)
		var queued int
		for _, job := range jobs.Jobs {
			if strings.Contains(string(job.EncodedArgs), uuid.UUID(created.ID).String()) {
				queued++
			}
		}
		require.Equal(t, 1, queued, "the import commits with its one job")
	})
}

// declareCCBill gives alpha a CCBill PSP, read by a DataLink pull when feed
// is set.
func declareCCBill(w *world, feed *httptest.Server) {
	m := w.cfg.Merchants["alpha"]
	psp := openrails.CCBillPSP{ClientAccnum: "945280", ClientSubacc: "0001", Environment: openrails.EnvironmentSandbox, Salt: "critical-ccbill-salt"}
	if feed != nil {
		psp.DatalinkUsername, psp.DatalinkPassword = "critical-datalink", "critical-datalink-password"
		w.cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{CCBillDataLinkURL: feed.URL}
	}
	m.PSPs = openrails.PSPs{"ccbill": psp.PSPConfig()}
	w.cfg.Merchants["alpha"] = m
}

// importCCBillAttribution imports a CCBill membership whose initial sale has
// an amount and whose rebill a day ago is declared without one.
func importCCBillAttribution(t *testing.T, w *world) billing.CustomerID {
	t.Helper()
	product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "ccbill-member", DisplayName: "CCBill member", Entitlements: []string{"ccbill:member"}})
	require.NoError(t, err)
	price, err := w.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "ccbill-monthly", PSP: "ccbill", FlexID: "critical-flex",
		Currency: "USD", UnitAmount: ptr(int64(10_000_000)), BillingIntervalHours: ptr(720)})
	require.NoError(t, err)
	customer := billing.CustomerID(uuid.New())
	rebill := w.clock.Now().Truncate(time.Second).Add(-24 * time.Hour)
	start := rebill.Add(-720 * time.Hour)
	book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "ccbill-member", Customer: customer, Price: price.ID, Rail: "ccbill", RailSubscriptionID: "0112233445", StartedAt: start, PaidThrough: &rebill,
			AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: "0112233445", Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: rebill}}},
		Transactions: []billing.DeclaredTransaction{
			{RailSubscriptionID: "0112233445", TransactionID: "0112233440", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start},
			{RailSubscriptionID: "0112233445", TransactionID: "0112233446", Type: "sale", Success: true, Currency: "USD", OccurredAt: rebill},
		}}
	result, err := w.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, result.Blocked, "%+v", result.Reasons)
	return customer
}

// dataLinkFeed is CCBill's DataLink as the pull reads it: the membership is
// active, and its rebill is in the export of any window holding it.
func dataLinkFeed(t *testing.T, rebill time.Time) *httptest.Server {
	t.Helper()
	mst := time.FixedZone("MST", -7*60*60)
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.URL.Path != "/data/main.cgi" {
			http.Error(out, "unexpected DataLink request", http.StatusBadRequest)
			return
		}
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			fmt.Fprintf(out, "\"ACTIVEMEMBERS\",\"945280\",\"critical-flex\",\"0112233445\",\"%s\",\"member\",\"member@critical.invalid\",\"1\",\"%s\",\"\"\n",
				rebill.Add(-720*time.Hour).Format("2006-01-02"), rebill.Add(720*time.Hour).Format("2006-01-02"))
			return
		}
		from, errFrom := time.ParseInLocation("20060102150405", r.Form.Get("startTime"), mst)
		until, errUntil := time.ParseInLocation("20060102150405", r.Form.Get("endTime"), mst)
		if errFrom != nil || errUntil != nil {
			http.Error(out, "unbounded DataLink export", http.StatusBadRequest)
			return
		}
		if !rebill.Before(from) && rebill.Before(until) {
			fmt.Fprintf(out, "REBILL,945280,0001,0112233445,%s,0112233446,10.00\n", rebill.UTC().Format("2006-01-02 15:04:05"))
		}
	}))
	t.Cleanup(server.Close)
	return server
}

type refreshMerchantJob struct {
	MerchantID uuid.UUID `json:"merchant_id"`
}

func (refreshMerchantJob) Kind() string { return "openrails.provider_refresh_merchant" }

// refreshMerchant runs the merchant's provider refresh through every window
// it has to read.
func refreshMerchant(t *testing.T, fleet *river.Client[pgx.Tx], merchant uuid.UUID) {
	t.Helper()
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	job, err := fleet.Insert(t.Context(), refreshMerchantJob{MerchantID: merchant}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	deadline := time.NewTimer(3 * time.Minute)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event == nil || event.Job.ID != job.Job.ID {
				continue
			}
			require.Equal(t, river.EventKindJobCompleted, event.Kind, "%+v", event.Job.Errors)
			return
		case <-deadline.C:
			t.Fatal("provider refresh did not finish its windows")
		}
	}
}

func unverifiedTransactions(t *testing.T, w *world) []billing.Finding {
	t.Helper()
	findings, err := w.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.import.transaction_unverified"})
	require.NoError(t, err)
	var open []billing.Finding
	for _, finding := range findings.Items {
		if finding.Status == "requires_review" || finding.Status == "reconcile_required" {
			open = append(open, finding)
		}
	}
	return open
}

func ccbillPayments(t *testing.T, w *world, customer billing.CustomerID) map[string]int64 {
	t.Helper()
	payments, err := w.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
	require.NoError(t, err)
	out := map[string]int64{}
	for _, payment := range payments.Items {
		out[payment.TransactionID] = payment.Amount
	}
	return out
}
