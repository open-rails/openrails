//go:build e2e && integration

package ci_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverkit "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Two hosts embed OpenRails on one database, each with a River schema of its
// own. A periodic pass is queued by one of them per period, a fleet's own
// state (its River jobs) is rescued by each, and when the holder stops the
// other takes over once the lease has expired.
func TestTwoFleetsQueueEachPeriodicPassOnce(t *testing.T) {
	f := newFixture(t)
	schemas := []string{f.schema, f.schema + "_host2"}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schemas[1]}.Sanitize()+" CASCADE")
	})
	leases := pgx.Identifier{f.schema, "periodic_leases"}.Sanitize()

	// the next clock hour at least a minute away; River's clock stands 12s before it
	now := time.Now()
	hour := now.Truncate(time.Hour).Add(time.Hour)
	if hour.Sub(now) < time.Minute {
		hour = hour.Add(time.Hour)
	}
	start := func(schema string, boundary time.Time) (rt *openrails.Engine, stop func()) {
		cfg := f.config()
		var err error
		cfg.Database.RiverSchema = schema
		rt, err = openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
		require.NoError(t, err)
		clock := riverClock{offset: boundary.Add(-12 * time.Second).Sub(time.Now())}
		fleet, err := riverkit.New(t.Context(), f.pool, &river.Config{
			ID: schema, Schema: schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
			TestOnly: true, Test: river.TestConfig{Time: clock},
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
		return rt, stop
	}
	// queued counts a kind's jobs scheduled in the boundary's first seconds, per fleet.
	queued := func(kind string, boundary time.Time) map[string]int {
		out := map[string]int{}
		for _, schema := range schemas {
			var n int
			require.NoError(t, f.pool.QueryRow(t.Context(),
				"SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE kind = $1 AND scheduled_at >= $2 AND scheduled_at < $3",
				kind, boundary, boundary.Add(5*time.Second)).Scan(&n))
			out[schema] = n
		}
		return out
	}
	leased := []string{
		riverjobs.RebillWatchArgs{}.Kind(), riverjobs.NMIHistoryArgs{}.Kind(), riverjobs.CleanupExpiredDataArgs{}.Kind(),
		riverjobs.SolanaCrankArgs{}.Kind(), riverjobs.OrderExpiryArgs{}.Kind(), riverjobs.ConvergeSweepArgs{}.Kind(),
		riverjobs.DelinquencyArgs{}.Kind(), riverjobs.CreditExpiryArgs{}.Kind(), riverjobs.StripeWebhookReconcileArgs{}.Kind(),
	}
	perFleet := []string{riverjobs.AdmissionDenialFlushArgs{}.Kind()}
	eventually := func(what string, ok func() bool) {
		t.Helper()
		require.Eventually(t, ok, 40*time.Second, 50*time.Millisecond, what)
	}

	// Both fleets tick at the hour: each leased pass is queued once across
	// them, and the fleet-local pass by both.
	_, stopOne := start(schemas[0], hour)
	_, stopTwo := start(schemas[1], hour)
	eventually("each leased pass is queued", func() bool {
		for _, kind := range leased {
			if q := queued(kind, hour); q[schemas[0]]+q[schemas[1]] < 1 {
				return false
			}
		}
		q := queued(perFleet[0], hour)
		return q[schemas[0]] == 1 && q[schemas[1]] == 1
	})
	time.Sleep(2 * time.Second)
	holders := map[string]string{}
	for _, kind := range leased {
		q := queued(kind, hour)
		require.Equal(t, 1, q[schemas[0]]+q[schemas[1]], "%s is queued once across both fleets: %v", kind, q)
		var holder string
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT holder FROM "+leases+" WHERE kind = $1", kind).Scan(&holder))
		require.Equal(t, 1, q[holder], "%s is queued by its lease holder", kind)
		holders[kind] = holder
	}

	// Both fleets stop; their leases are the dead holder's and lapse a few
	// seconds from now. A fleet that starts afterwards, ticking a period later,
	// finds them expired and queues the passes.
	stopOne()
	stopTwo()
	_, err := f.pool.Exec(t.Context(), "UPDATE "+leases+" SET holder = $1, expires_at = now() + interval '5 seconds'", schemas[0])
	require.NoError(t, err)
	next := hour.Add(time.Hour)
	_, stopThree := start(schemas[1], next)
	defer stopThree()
	eventually("the survivor takes the passes over", func() bool {
		for _, kind := range leased {
			if queued(kind, next)[schemas[1]] != 1 {
				return false
			}
		}
		return true
	})
}
