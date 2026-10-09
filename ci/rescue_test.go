//go:build e2e && integration

package ci_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// Each process has a fixed insertion clock, so crossing a uniqueness period
// does not require a wall-clock sleep. River still executes against PostgreSQL.
type rescueClock struct{ at time.Time }

func (c rescueClock) Now() time.Time       { return c.at }
func (c rescueClock) NowOrNil() *time.Time { return &c.at }

func TestRescuerSurvivesItsOwnInterruptedJob(t *testing.T) {
	f := newFixture(t)
	jobsTable := pgx.Identifier{f.schema, "river_job"}.Sanitize()
	start := func(at time.Time) func() {
		rt, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool})
		require.NoError(t, err)
		jobs, err := riverkit.New(t.Context(), f.pool, &river.Config{
			Schema: f.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
			TestOnly: true, Test: river.TestConfig{Time: rescueClock{at}},
			FetchCooldown: 5 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond,
		}, rt.RiverJobs())
		require.NoError(t, err)
		require.NoError(t, jobs.Start(context.WithoutCancel(t.Context())))
		stop := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, jobs.Stop(ctx))
			require.NoError(t, rt.Close(ctx))
		}
		t.Cleanup(stop)
		return stop
	}
	completed := func(want int) int64 {
		var latest int64
		require.Eventually(t, func() bool {
			var count int
			err := f.pool.QueryRow(t.Context(), "SELECT count(*), coalesce(max(id), 0) FROM "+jobsTable+" WHERE kind = 'openrails.job_rescue' AND state = 'completed'").Scan(&count, &latest)
			return err == nil && count == want
		}, 10*time.Second, 20*time.Millisecond, "expected %d completed rescue jobs", want)
		return latest
	}

	// Keep River ahead of database wall time so a rescued job scheduled by
	// SQL now() is immediately eligible under its stubbed fetch clock.
	priorPeriod := time.Now().UTC().Truncate(time.Minute).Add(5 * time.Minute)
	stop := start(priorPeriod)
	orphan := completed(1)
	stop()
	// Model process loss during the rescuer itself, preserving River's actual
	// production-generated uniqueness key. Its stopped heartbeat proves death.
	_, err := f.pool.Exec(t.Context(), "UPDATE "+jobsTable+" SET state = 'running', finalized_at = NULL, attempted_at = now() - interval '10 minutes' WHERE id = $1", orphan)
	require.NoError(t, err)

	nextPeriod := priorPeriod.Add(time.Minute)
	stop = start(nextPeriod)
	completed(2) // The new pass runs and the formerly orphaned rescuer also runs.
	stop()
	var state string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT state FROM "+jobsTable+" WHERE id = $1", orphan).Scan(&state))
	require.Equal(t, "completed", state)

	// A completed pass cannot suppress RunOnStart even within the same minute.
	stop = start(nextPeriod)
	completed(3)
	stop()
}

// Migrate creates River's tables in Config.RiverSchema; New refuses a schema
// without them, naming the call. Neither New nor Start runs DDL.
func TestNewNamesTheMissingRiverMigration(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.RiverSchema = f.schema + "_jobs"
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{cfg.RiverSchema}.Sanitize()+" CASCADE")
	})
	_, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.ErrorContains(t, err, "call openrails.Migrate(ctx, pool, cfg) before openrails.New")
	var schemas int
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_namespace WHERE nspname = $1", cfg.RiverSchema).Scan(&schemas))
	require.Zero(t, schemas, "New runs no DDL")

	require.NoError(t, openrails.Migrate(t.Context(), f.pool, cfg))
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	require.NoError(t, client.Close(t.Context()))
}

// Start without options runs OpenRails' own River in Config.RiverSchema. Jobs
// queued before it wait there; Ready fails until it runs. Cancelling Start's
// context stops nothing; Close stops what Start started.
func TestStartRunsItsOwnRiver(t *testing.T) {
	f := newFixture(t)
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	require.ErrorContains(t, client.Ready(t.Context()), "River is not running")

	started, cancel := context.WithCancel(t.Context())
	require.NoError(t, client.Start(started))
	cancel()
	require.ErrorContains(t, client.Start(t.Context()), "already started")
	require.Eventually(t, func() bool { return client.Ready(t.Context()) == nil }, 10*time.Second, 50*time.Millisecond, "a cancelled Start context leaves River running")
	require.NoError(t, client.Close(t.Context()))
	require.Error(t, client.Ready(t.Context()))
}

// WithRiverClient takes only the fleet built with this client's RiverJobs in
// Config.RiverSchema; once RiverJobs went to a fleet, OpenRails' own River is
// refused.
func TestStartWithTheHostFleet(t *testing.T) {
	f := newFixture(t)
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	other, err := river.NewClient(riverpgxv5.New(f.pool), &river.Config{Schema: f.schema})
	require.NoError(t, err)
	require.ErrorContains(t, client.Start(t.Context(), openrails.WithRiverClient(other)), "WithRiverClient takes the fleet riverhelpers.New built with RiverJobs")
	require.ErrorContains(t, client.Start(t.Context(), openrails.WithRiverClient(nil)), "WithRiverClient requires a River client")

	fleet, err := riverkit.New(t.Context(), f.pool, &river.Config{Schema: f.schema}, client.RiverJobs())
	require.NoError(t, err)
	require.ErrorContains(t, client.Start(t.Context()), "RiverJobs is composed into a host fleet; pass it to Start with WithRiverClient")
	require.NoError(t, client.Ready(t.Context()), "a bound host fleet is ready: the host starts it")
	require.NoError(t, client.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.ErrorContains(t, client.Start(t.Context(), openrails.WithRiverClient(fleet)), "already started")

	elsewhere, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = elsewhere.Close(context.Background()) })
	_, err = riverkit.New(t.Context(), f.pool, &river.Config{Schema: "public"}, elsewhere.RiverJobs())
	require.ErrorContains(t, err, `River fleet schema "public" differs from Config.RiverSchema "`+f.schema+`"`)
	require.ErrorContains(t, elsewhere.Start(t.Context()), "composition failed")
}
