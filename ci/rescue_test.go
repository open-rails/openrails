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

func TestRescuerSurvivesItsOwnInterruptedJob(t *testing.T) {
	f := newFixture(t)
	jobsTable := pgx.Identifier{f.schema, "river_job"}.Sanitize()
	start := func() func() {
		rt, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
		require.NoError(t, err)
		jobs, err := riverkit.New(t.Context(), f.pool, &river.Config{
			Schema: f.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
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

	stop := start()
	orphan := completed(1)
	stop()
	// Model process loss during a prior-period rescuer. Its stopped heartbeat
	// proves death; the old period's uniqueness key cannot cover the new pass.
	_, err := f.pool.Exec(t.Context(), "UPDATE "+jobsTable+" SET state = 'running', finalized_at = NULL, created_at = now() - interval '10 minutes', scheduled_at = now() - interval '10 minutes', attempted_at = now() - interval '10 minutes', unique_key = NULL WHERE id = $1", orphan)
	require.NoError(t, err)

	stop = start()
	completed(2) // The new pass runs and the formerly orphaned rescuer also runs.
	stop()
	var state string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT state FROM "+jobsTable+" WHERE id = $1", orphan).Scan(&state))
	require.Equal(t, "completed", state)

	// A recent completed pass suppresses a same-minute restart. Both the
	// rescued original and its recovery pass remain the only two rows.
	stop = start()
	require.Never(t, func() bool {
		var count int
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+jobsTable+" WHERE kind = 'openrails.job_rescue'").Scan(&count))
		return count != 2
	}, 2*time.Second, 20*time.Millisecond)
	stop()
}

// New creates River's tables in Config.Database.RiverSchema, a schema of
// their own here, before it binds the producer that queues jobs there.
func TestNewCreatesTheRiverSchema(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Database.RiverSchema = f.schema + "_jobs"
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{cfg.Database.RiverSchema}.Sanitize()+" CASCADE")
	})
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	require.NoError(t, client.Close(t.Context()))
	var jobs *string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT to_regclass($1)::text", cfg.Database.RiverSchema+".river_job").Scan(&jobs))
	require.NotNil(t, jobs, "River's tables are in the River schema")
}

// Start without options runs OpenRails' own River in
// Config.Database.RiverSchema. Jobs queued before it wait there; Ready fails
// until it runs. Cancelling Start's context stops nothing; Close stops what
// Start started.
func TestStartRunsItsOwnRiver(t *testing.T) {
	f := newFixture(t)
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
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
// Config.Database.RiverSchema; once RiverJobs went to a fleet, OpenRails' own
// River is refused.
func TestStartWithTheHostFleet(t *testing.T) {
	f := newFixture(t)
	client, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
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

	elsewhere, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = elsewhere.Close(context.Background()) })
	_, err = riverkit.New(t.Context(), f.pool, &river.Config{Schema: "public"}, elsewhere.RiverJobs())
	require.ErrorContains(t, err, `River fleet schema "public" differs from Config.Database.RiverSchema "`+f.schema+`"`)
	require.ErrorContains(t, elsewhere.Start(t.Context()), "composition failed")
}
