//go:build greenfield && integration

package greenfield_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
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
		rt, err := embed.New(t.Context(), embed.Options{
			Config: &config.Config{
				TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly,
				DB: &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
			},
			PGXPool: f.pool, River: embed.RiverFromHost(),
		})
		require.NoError(t, err)
		jobs, err := riverkit.New(t.Context(), f.pool, &river.Config{
			Schema: f.schema, Queues: map[string]river.QueueConfig{embed.QueueBilling: {MaxWorkers: 2}},
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
