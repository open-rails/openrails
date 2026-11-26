//go:build integration

package critical_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// withTaggedSessions gives the engine a direct session configuration whose
// sessions carry the world's schema as their application name, so a scenario
// sees every session the engine dials. Its pool is the world's, in the
// pooler-safe mode a session configuration requires.
func withTaggedSessions(w *world) {
	cfg := w.pool.Config()
	cfg.ConnConfig.RuntimeParams["application_name"] = w.schema
	w.deps.PostgresSession = cfg
	pooled := w.pool.Config()
	pooled.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe
	pool, err := pgxpool.NewWithConfig(context.Background(), pooled)
	if err != nil {
		panic(err)
	}
	w.deps.Postgres = pool
}

// heldSessions counts the world's direct sessions that are not listening:
// those a lock would hold.
func heldSessions(t *testing.T, w *world) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity
WHERE application_name = $1 AND query <> '' AND query NOT LIKE 'LISTEN %'`, w.schema).Scan(&n))
	return n
}

// worldDB is the world's database as a host's one-off process opens it,
// sessions tagged as the engine's when it has a session configuration.
func worldDB(t *testing.T, w *world) *db.DB {
	t.Helper()
	database, err := db.NewWithPGXPoolAndSessions(slog.Default(), w.deps.Postgres, w.schema, w.deps.PostgresSession)
	require.NoError(t, err)
	return database
}

// liveLeases counts the unexpired leases of kind in table.
func liveLeases(t *testing.T, w *world, table, kind string) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, table}.Sanitize()+" WHERE kind = $1 AND expires_at > now()", kind).Scan(&n))
	return n
}

// pullConcurrently starts n pulls of the merchant at once and returns those
// that took its lease.
func pullConcurrently(t *testing.T, database *db.DB, merchant uuid.UUID, n int) []*db.RunLease {
	t.Helper()
	var mu sync.Mutex
	var held []*db.RunLease
	var errs []error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range n {
		wg.Go(func() {
			<-start
			lease, err := reconcile.LockMerchantPull(t.Context(), database, merchant)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
			} else if lease != nil {
				held = append(held, lease)
			}
		})
	}
	close(start)
	wg.Wait()
	require.Empty(t, errs)
	return held
}

func TestMerchantPullLease(t *testing.T) {
	t.Run("OR-J38-S83/one-pull-of-a-merchant-runs-the-others-skip-and-none-holds-a-session", func(t *testing.T) {
		w := newWorld(t, withTaggedSessions)
		fleet := quietFleet(t, w)
		database := worldDB(t, w)
		merchant := w.alpha.MerchantID().UUID()

		held := pullConcurrently(t, database, merchant, 8)
		require.Len(t, held, 1, "one pull of a merchant runs at a time")
		pull := held[0]
		defer pull.Release()
		require.Equal(t, 1, liveLeases(t, w, "merchant_leases", string(db.LeasePull)))
		require.Zero(t, heldSessions(t, w), "the running pull holds a lease row, not a session")
		other, err := reconcile.LockMerchantPull(t.Context(), worldDB(t, w), w.beta.MerchantID().UUID())
		require.NoError(t, err)
		require.NotNil(t, other, "another merchant's pull runs beside it")
		other.Release()

		// The engine's refresh of the merchant waits for the running pull.
		events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed, river.EventKindJobSnoozed)
		defer unsubscribe()
		job, err := fleet.Insert(t.Context(), riverjobs.ProviderRefreshRequestArgs{MerchantID: merchant}, nil)
		require.NoError(t, err)
		awaitJobEvent(t, events, job.Job.ID, river.EventKindJobSnoozed)
		pull.Release()
		awaitJobEvent(t, events, job.Job.ID, river.EventKindJobCompleted)
		require.Zero(t, liveLeases(t, w, "merchant_leases", string(db.LeasePull)), "the refresh released its lease")
	})
	t.Run("OR-J38-S84/a-pull-outlives-its-lease-period-and-a-dead-holders-lease-lapses", func(t *testing.T) {
		w := newWorld(t)
		database := worldDB(t, w)
		merchant := w.alpha.MerchantID().UUID()
		const ttl = 3 * time.Second

		// A long pull renews its lease.
		long, err := database.TryMerchantLease(t.Context(), merchant, db.LeasePull, ttl)
		require.NoError(t, err)
		require.NotNil(t, long)
		time.Sleep(3 * ttl)
		require.NoError(t, long.Context().Err(), "a pull past one lease period keeps its lease")
		require.Empty(t, pullConcurrently(t, database, merchant, 4), "and another pull still skips")
		long.Release()
		next := pullConcurrently(t, database, merchant, 4)
		require.Len(t, next, 1, "its release frees the merchant's next pull")
		next[0].Release()

		// A holder that dies stops renewing; its lease lapses on its own.
		alive, die := context.WithCancel(t.Context())
		dead, err := database.TryMerchantLease(alive, merchant, db.LeasePull, ttl)
		require.NoError(t, err)
		require.NotNil(t, dead)
		die()
		require.Empty(t, pullConcurrently(t, database, merchant, 4), "a dead holder's lease holds until it lapses")
		require.Eventually(t, func() bool {
			lease, err := reconcile.LockMerchantPull(t.Context(), database, merchant)
			if err != nil || lease == nil {
				return false
			}
			lease.Release()
			return true
		}, 4*ttl, 100*time.Millisecond, "the next pull takes the lapsed lease")
	})
}

// awaitJobEvent waits for the job's event of kind.
func awaitJobEvent(t *testing.T, events <-chan *river.Event, id int64, kind river.EventKind) *rivertype.JobRow {
	t.Helper()
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if event == nil || event.Job.ID != id {
				continue
			}
			if event.Kind == kind {
				return event.Job
			}
			require.NotEqual(t, river.EventKindJobFailed, event.Kind, "job %d failed: %v", id, event.Job.Errors)
		case <-deadline.C:
			t.Fatalf("job %d: no %s event", id, kind)
		}
	}
}
