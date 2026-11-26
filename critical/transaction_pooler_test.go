//go:build integration

package critical_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
)

// poolerServers is the pool size PgBouncer keeps for the critical database
// (scripts/ci-services.sh): that many concurrent transactions reach every
// server connection it hands out.
const poolerServers = 4

// openrailsSettings are every setting OpenRails' SQL sets. Each is
// transaction-local, so none may be seen outside the transaction that set it.
var openrailsSettings = []string{
	db.MerchantGUC, db.MRRCauseGUC,
	"openrails.subscription_decision", "openrails.subscription_history",
	"openrails.retention_table", "openrails.billing_restore_id",
	"openrails.catalog_batch_merchant_id", "openrails.catalog_revision_merchant_ids",
	"openrails.event_log_flushed",
}

func poolerDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_CRITICAL_PGBOUNCER_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_PGBOUNCER_DSN must name PgBouncer in transaction mode before OPENRAILS_CRITICAL_DSN's database; critical scenarios never skip")
	return dsn
}

// behindPgBouncer serves the world's engine through PgBouncer in transaction
// mode, server-side prepared statements off, as production does: its pool in
// cache_describe mode and a direct session configuration whose sessions carry
// the world's schema as their application name.
func behindPgBouncer(t *testing.T) func(*world) {
	t.Helper()
	dsn := poolerDSN(t)
	return func(w *world) {
		cfg, err := pgxpool.ParseConfig(dsn)
		require.NoError(t, err)
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
		cfg.MaxConns = 16
		pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
		require.NoError(t, err)
		w.deps.Postgres = pool
		session := w.pool.Config()
		session.ConnConfig.RuntimeParams["application_name"] = w.schema
		w.deps.PostgresSession = session
	}
}

// serverSettings holds poolerServers transactions open through PgBouncer at
// once, so each is on its own server connection and together they reach every
// one, and reads OpenRails' settings on each: what a client the pooler hands
// that connection to next would see.
func serverSettings(t *testing.T) map[int32]map[string]string {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(poolerDSN(t))
	require.NoError(t, err)
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.MaxConns = poolerServers
	probe, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	defer probe.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type seen struct {
		pid      int32
		settings map[string]string
		err      error
	}
	results, release := make(chan seen, 2*poolerServers), make(chan struct{})
	var wg sync.WaitGroup
	for range poolerServers {
		wg.Go(func() {
			out := seen{settings: map[string]string{}}
			defer func() { results <- out }()
			tx, err := probe.Begin(ctx)
			if out.err = err; err != nil {
				return
			}
			defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
			if out.err = tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&out.pid); out.err != nil {
				return
			}
			for _, name := range openrailsSettings {
				var value string
				if out.err = tx.QueryRow(ctx, "SELECT coalesce(current_setting($1, true), '')", name).Scan(&value); out.err != nil {
					return
				}
				out.settings[name] = value
			}
			results <- seen{pid: -1}
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	}
	ready := 0
	for ready < poolerServers {
		select {
		case r := <-results:
			require.NoError(t, r.err)
			require.Equal(t, int32(-1), r.pid, "a probe ended before every server connection was held")
			ready++
		case <-ctx.Done():
			t.Fatal("PgBouncer did not hand out every server connection at once")
		}
	}
	close(release)
	wg.Wait()
	close(results)
	out := map[int32]map[string]string{}
	for r := range results {
		require.NoError(t, r.err)
		out[r.pid] = r.settings
	}
	return out
}

// requireNoSettingSurvives requires every server connection PgBouncer holds to
// carry none of OpenRails' settings.
func requireNoSettingSurvives(t *testing.T) {
	t.Helper()
	servers := serverSettings(t)
	require.Len(t, servers, poolerServers, "the probe reached every server connection")
	for pid, settings := range servers {
		for name, value := range settings {
			require.Empty(t, value, "server connection %d kept %s past its transaction", pid, name)
		}
	}
}

// directListeners are the pids of the world's direct sessions listening on a
// channel whose name contains like. OpenRails' listeners ping by listening
// again; River's pings with "-- ping".
func directListeners(t *testing.T, w *world, like string) []int32 {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), `SELECT pid FROM pg_stat_activity
WHERE application_name = $1 AND query LIKE 'LISTEN %' AND query LIKE '%' || $2 || '%' ORDER BY pid`, w.schema, like)
	require.NoError(t, err)
	pids, err := pgx.CollectRows(rows, pgx.RowTo[int32])
	require.NoError(t, err)
	return pids
}

// riverListener is the pid of River's direct listening session, 0 without
// one.
func riverListener(t *testing.T, w *world) int32 {
	t.Helper()
	var pid int32
	err := w.pool.QueryRow(t.Context(), `SELECT pid FROM pg_stat_activity
WHERE application_name = $1 AND (query LIKE 'LISTEN %river%' OR query LIKE '-- ping%')`, w.schema).Scan(&pid)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0
	}
	require.NoError(t, err)
	return pid
}

// workerChannels are the channels OpenRails' workers listen on.
var workerChannels = []string{"openrails_unverified", "openrails_solana_pay", "openrails_customer_messages", "openrails_dispute_responses", "openrails_mrr_risk"}

func TestTransactionPooler(t *testing.T) {
	t.Run("OR-J04-S86/behind-pgbouncer-each-statement-runs-as-its-merchant-and-no-setting-survives-its-transaction", func(t *testing.T) {
		w := newWorld(t, behindPgBouncer(t))
		database := embedded.Graph(w.engine).Runtime.DB
		merchants := []billing.MerchantID{w.alpha.MerchantID(), w.beta.MerchantID()}

		// Statements, transactions and committed custody of two merchants at
		// once share PgBouncer's few server connections; each runs as its own.
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for i := range 24 {
			mid := merchants[i%2]
			wg.Go(func() {
				errs <- database.RunInMerchantConn(merchant.WithID(t.Context(), mid), func(ctx context.Context) error {
					var got string
					if err := database.Qx(ctx).QueryRow(ctx, "SELECT current_setting('openrails.merchant_id')").Scan(&got); err != nil {
						return err
					}
					if got != mid.String() {
						return errors.New("a statement ran as merchant " + got + ", not " + mid.String())
					}
					if err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
						return tx.QueryRow(ctx, "SELECT current_setting('openrails.merchant_id')").Scan(&got)
					}); err != nil {
						return err
					}
					if got != mid.String() {
						return errors.New("a transaction ran as merchant " + got + ", not " + mid.String())
					}
					return database.DataPool().CommittedMerchantTx(ctx, mid, func(ctx context.Context, tx pgx.Tx) error {
						if err := tx.QueryRow(ctx, "SELECT current_setting('openrails.merchant_id')").Scan(&got); err != nil {
							return err
						}
						if got != mid.String() {
							return errors.New("custody ran as merchant " + got + ", not " + mid.String())
						}
						return nil
					})
				})
			})
		}
		// Catalog applies set their batch and revision settings, and the
		// merchant's, in their transactions.
		for i, client := range []*openrails.Client{w.alpha, w.beta} {
			wg.Go(func() {
				document, err := catalog.ParseApplicationYAML([]byte("schema_version: 1\nproducts:\n  pooled:\n    display_name: Pooled\n    prices:\n      monthly: {amount: " + []string{"10", "20"}[i] + " USD, billing_interval: 1 month}\n"))
				if err == nil {
					_, err = client.ApplyCatalog(t.Context(), document, billing.ApplyCatalogParams{})
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		for i, client := range []*openrails.Client{w.alpha, w.beta} {
			products, err := client.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"pooled"}})
			require.NoError(t, err)
			require.Len(t, products.Items, 1, "each merchant's apply landed on its own catalog")
			prices, err := client.ListPrices(t.Context(), billing.PriceListParams{ProductID: products.Items[0].ID})
			require.NoError(t, err)
			require.Len(t, prices.Items, 1)
			require.Equal(t, int64((i+1)*10_000_000), prices.Items[0].UnitAmount)
		}
		requireNoSettingSurvives(t)
	})

	t.Run("OR-J04-S87/behind-pgbouncer-river-works-through-the-pooler-and-a-direct-listener-wakes-it", func(t *testing.T) {
		w := newWorld(t, behindPgBouncer(t))
		chores := &hostChoreWorker{ran: make(chan int64, 4)}
		workers := river.NewWorkers()
		river.AddWorker(workers, chores)
		// Producers poll every 30 seconds: a job that runs within seconds was
		// woken by the listener.
		fleet, err := riverhelpers.NewWithSession(t.Context(), w.deps.Postgres, w.deps.PostgresSession, &river.Config{
			Schema: w.schema + "_river", Workers: workers, FetchPollInterval: 30 * time.Second,
			Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}, "host": {MaxWorkers: 1}},
		}, w.engine.RiverJobs())
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, fleet.StopAndCancel(ctx))
		})
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))

		// River and every OpenRails worker listen on direct sessions.
		listening := func() (pids []int32) {
			if pid := riverListener(t, w); pid != 0 {
				pids = append(pids, pid)
			}
			for _, channel := range workerChannels {
				if found := directListeners(t, w, channel); len(found) == 1 {
					pids = append(pids, found[0])
				}
			}
			return pids
		}
		var listeners []int32
		require.Eventually(t, func() bool { listeners = listening(); return len(listeners) == 1+len(workerChannels) }, 15*time.Second, 100*time.Millisecond,
			"River and each worker listen on a direct session")
		awaitChore := func(id int64) {
			t.Helper()
			select {
			case ran := <-chores.ran:
				require.Equal(t, id, ran)
			case <-time.After(10 * time.Second):
				t.Fatal("a job queued through PgBouncer waited for River's poll: its notification never woke the producer")
			}
		}
		for range 2 {
			job, err := fleet.Insert(t.Context(), hostChore{}, &river.InsertOpts{Queue: "host"})
			require.NoError(t, err)
			awaitChore(job.Job.ID)
		}

		// A dead listener is redialed, and notifications wake River again.
		for _, pid := range listeners {
			_, err := w.pool.Exec(t.Context(), "SELECT pg_terminate_backend($1)", pid)
			require.NoError(t, err)
		}
		require.Eventually(t, func() bool {
			now := listening()
			if len(now) != len(listeners) {
				return false
			}
			for _, pid := range now {
				if slices.Contains(listeners, pid) {
					return false
				}
			}
			return true
		}, 40*time.Second, 200*time.Millisecond, "every listener redialed its session")
		job, err := fleet.Insert(t.Context(), hostChore{}, &river.InsertOpts{Queue: "host"})
		require.NoError(t, err)
		awaitChore(job.Job.ID)

		// OpenRails' own jobs run through the pooler: the periodic passes
		// River starts with complete.
		for _, kind := range []string{"openrails.cleanup_expired_data", "openrails.billing_pass"} {
			require.Eventually(t, func() bool {
				jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds(kind))
				require.NoError(t, err)
				for _, j := range jobs.Jobs {
					require.Empty(t, j.Errors, "%s failed through PgBouncer", kind)
					if j.FinalizedAt != nil {
						return true
					}
				}
				return false
			}, 60*time.Second, 250*time.Millisecond, "%s completes through PgBouncer", kind)
		}
		requireNoSettingSurvives(t)
	})

	t.Run("OR-J04-S88/behind-pgbouncer-session-locks-leases-and-destructive-holds-still-exclude", func(t *testing.T) {
		r := newReservationWorld(t, behindPgBouncer(t))
		database, err := db.NewWithPGXPoolAndSessions(slog.Default(), r.deps.Postgres, r.schema, r.deps.PostgresSession)
		require.NoError(t, err)
		mid := r.alpha.MerchantID()

		// A session lock waits for its holder, both on direct sessions.
		exclusive := func(what string, lock func(context.Context) (func(), error)) {
			t.Helper()
			release, err := lock(t.Context())
			require.NoError(t, err)
			waiting := make(chan func(), 1)
			go func() {
				second, err := lock(t.Context())
				if err != nil {
					second = func() {}
				}
				waiting <- second
			}()
			select {
			case second := <-waiting:
				second()
				t.Fatalf("a second %s lock was granted while the first was held", what)
			case <-time.After(time.Second):
			}
			release()
			select {
			case second := <-waiting:
				second()
			case <-time.After(10 * time.Second):
				t.Fatalf("the second %s lock never followed the first's release", what)
			}
		}
		exclusive("usage policy", func(ctx context.Context) (func(), error) { return database.LockUsagePolicy(ctx, mid) })
		exclusive("billing import", func(ctx context.Context) (func(), error) { return reconcile.LockDeclaredBooks(ctx, database, mid) })

		// A merchant pull's lease is a row through the pooler: one holder.
		lease, err := reconcile.LockMerchantPull(t.Context(), database, mid.UUID())
		require.NoError(t, err)
		require.NotNil(t, lease)
		other, err := reconcile.LockMerchantPull(t.Context(), database, mid.UUID())
		require.NoError(t, err)
		require.Nil(t, other, "one pull of a merchant at a time")
		lease.Release()

		// Four revocations at once by an actor allowed two an hour: their
		// admissions take turns through the pooler, so at most two revoke and
		// the rest are held; a hold also stops an admitted one at its start.
		statuses := make(chan int, len(r.grants))
		var wg sync.WaitGroup
		for _, grant := range r.grants {
			wg.Go(func() {
				status, _, _ := r.revoke(t, grant)
				statuses <- status
			})
		}
		wg.Wait()
		close(statuses)
		counts := map[int]int{}
		for status := range statuses {
			counts[status]++
		}
		require.Equal(t, len(r.grants), counts[http.StatusOK]+counts[http.StatusTooManyRequests], "%v", counts)
		require.LessOrEqual(t, counts[http.StatusOK], 2, "the actor's limit admits at most two")
		var active []billing.CreditGrant
		for _, grant := range r.grants {
			got, err := r.alpha.GetCreditGrant(t.Context(), grant.ID)
			require.NoError(t, err)
			if got.State == billing.CreditGrantActive {
				active = append(active, grant)
			}
		}
		require.Len(t, active, len(r.grants)-counts[http.StatusOK], "each answer matches its grant")
		status, _, raw := r.revoke(t, active[0])
		require.Equal(t, http.StatusTooManyRequests, status, "the actor stays held: %s", raw)
		requireNoSettingSurvives(t)
	})

	t.Run("OR-J04-S89/behind-pgbouncer-without-prepared-statements-no-statement-fails-and-a-preparing-pool-is-refused", func(t *testing.T) {
		w := newWorld(t, behindPgBouncer(t))
		database := embedded.Graph(w.engine).Runtime.DB

		// Many clients reuse a handful of statements over four server
		// connections: a statement prepared on one would be missing on the
		// next, and none is.
		var wg sync.WaitGroup
		errs := make(chan error, 64)
		for i := range 64 {
			client := []*openrails.Client{w.alpha, w.beta}[i%2]
			wg.Go(func() {
				_, err := client.ListProducts(t.Context(), billing.ProductListParams{})
				if err == nil {
					_, err = client.ListCustomers(t.Context(), billing.CustomerListParams{})
				}
				if err == nil {
					err = database.RunInMerchantConn(merchant.WithID(t.Context(), client.MerchantID()), func(ctx context.Context) error {
						return database.AssertMerchantScope(ctx, "pooled read")
					})
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}

		// A pool that keeps prepared statements is refused before it can fail.
		cfg, err := pgxpool.ParseConfig(poolerDSN(t))
		require.NoError(t, err)
		preparing, err := pgxpool.NewWithConfig(t.Context(), cfg)
		require.NoError(t, err)
		defer preparing.Close()
		refused := w.cfg
		refused.Database.Schema = criticalSchema()
		_, err = openrails.New(t.Context(), refused, openrails.Deps{Postgres: preparing, PostgresSession: w.deps.PostgresSession, Clock: w.clock, FXTransport: fixedFX(t)})
		require.Error(t, err)
		require.True(t, strings.Contains(err.Error(), "DefaultQueryExecMode"), err.Error())
		requireNoSettingSurvives(t)
	})
}
