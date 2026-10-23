//go:build e2e && integration

package ci_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Leadership changes preserve elapsed deadlines: a recent run is not repeated,
// while retained overdue work catches up without waiting for another interval.
func TestPeriodicWorkKeepsItsBoundariesAcrossLeaders(t *testing.T) {
	f := newFixture(t)
	jobs := pgx.Identifier{f.schema, "river_job"}.Sanitize()
	leaders := pgx.Identifier{f.schema, "river_leader"}.Sanitize()
	start := func(id string) (stop func()) {
		rt, err := openrails.New(t.Context(), f.config(), openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
		require.NoError(t, err)
		fleet, err := riverkit.New(t.Context(), f.pool, &river.Config{
			ID: id, Schema: f.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
			FetchCooldown: 5 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond,
		}, rt.RiverJobs())
		require.NoError(t, err)
		require.NoError(t, fleet.Start(context.WithoutCancel(t.Context())))
		stopped := false
		stop = func() {
			if stopped {
				return
			}
			stopped = true
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			require.NoError(t, fleet.Stop(ctx))
			require.NoError(t, rt.Close(ctx))
		}
		t.Cleanup(stop)
		return stop
	}
	leads := func(id string) {
		t.Helper()
		require.Eventually(t, func() bool {
			var leader string
			err := f.pool.QueryRow(t.Context(), "SELECT leader_id FROM "+leaders).Scan(&leader)
			return err == nil && leader == id
		}, 30*time.Second, 20*time.Millisecond, "%s leads River", id)
	}
	kinds := []string{riverjobs.DunningArgs{}.Kind(), riverjobs.BalanceWatchArgs{}.Kind(), riverjobs.CleanupExpiredDataArgs{}.Kind()}
	counts := func() map[string]int {
		out := map[string]int{}
		rows, err := f.pool.Query(t.Context(), "SELECT metadata->>'river:periodic_job_id', count(*) FROM "+jobs+" WHERE metadata->>'river:periodic_job_id' = ANY($1::text[]) GROUP BY 1", kinds)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var kind string
			var count int
			require.NoError(t, rows.Scan(&kind, &count))
			out[kind] = count
		}
		require.NoError(t, rows.Err())
		return out
	}
	want := map[string]int{}
	for _, kind := range kinds {
		want[kind] = 1
	}
	stopFirst := start("first")
	leads("first")
	require.Eventually(t, func() bool { return equalPeriodicCounts(counts(), want) }, 20*time.Second, 20*time.Millisecond)
	stopSecond := start("second")
	stopFirst()
	leads("second")
	require.Never(t, func() bool { return !equalPeriodicCounts(counts(), want) }, 2*time.Second, 20*time.Millisecond, "leader takeover must not repeat recent work")
	stopSecond()

	agePeriodicHistory(t, f.pool, f.schema, kinds)
	_, err := f.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{f.schema, "periodic_leases"}.Sanitize()+" SET expires_at = now() - interval '1 second'")
	require.NoError(t, err)
	start("third")
	leads("third")
	for _, kind := range kinds {
		want[kind] = 2
	}
	require.Eventually(t, func() bool { return equalPeriodicCounts(counts(), want) }, 20*time.Second, 20*time.Millisecond, "overdue work catches up on takeover")
}

func equalPeriodicCounts(got, want map[string]int) bool {
	for kind, count := range want {
		if got[kind] != count {
			return false
		}
	}
	return true
}

// Model retained jobs from an earlier period while keeping their stable IDs.
// Old uniqueness keys belong to that earlier period, not the current one.
func agePeriodicHistory(t *testing.T, pool *pgxpool.Pool, schema string, kinds []string) {
	t.Helper()
	_, err := pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{schema, "river_job"}.Sanitize()+` SET
		created_at = now() - interval '2 days', scheduled_at = now() - interval '2 days',
		attempted_at = now() - interval '2 days', unique_key = NULL
		WHERE metadata->>'river:periodic_job_id' = ANY($1::text[])`, kinds)
	require.NoError(t, err)
}
