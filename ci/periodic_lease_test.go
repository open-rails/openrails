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
// own. Fleets declaring the same merchants queue each periodic pass once
// between them, and when the holder stops another takes over once the lease
// has expired. Fleets declaring different merchants each queue every pass:
// credentials only one holds must never wait on the other's lease.
func TestFleetsOnOneMerchantSetQueueEachPeriodicPassOnce(t *testing.T) {
	periodicLeases(t, []string{"shop-a"}, []string{"shop-a"})
}

func TestFleetsOnDifferentMerchantSetsEachQueueEveryPeriodicPass(t *testing.T) {
	periodicLeases(t, []string{"shop-a"}, []string{"shop-b"})
}

func periodicLeases(t *testing.T, merchants0, merchants1 []string) {
	f := newFixture(t)
	shared := len(merchants0) == len(merchants1) && merchants0[0] == merchants1[0]
	sets := [][]string{merchants0, merchants1}
	schemas := []string{f.schema, f.schema + "_host2"}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schemas[1]}.Sanitize()+" CASCADE")
	})
	leases := pgx.Identifier{f.schema, "periodic_leases"}.Sanitize()

	start := func(schema string, merchants []string) (rt *openrails.Engine, stop func()) {
		cfg := f.config()
		cfg.Merchants = map[string]openrails.MerchantDeclaration{}
		for _, slug := range merchants {
			cfg.Merchants[slug] = openrails.MerchantDeclaration{DisplayName: slug}
		}
		var err error
		cfg.Database.RiverSchema = schema
		rt, err = openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
		require.NoError(t, err)
		fleet, err := riverkit.New(t.Context(), f.pool, &river.Config{
			ID: schema, Schema: schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
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
	// queued counts only this periodic schedule, excluding explicit events.
	queued := func(kind string) map[string]int {
		out := map[string]int{}
		for _, schema := range schemas {
			var n int
			require.NoError(t, f.pool.QueryRow(t.Context(),
				"SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE metadata->>'river:periodic_job_id' = $1", kind).Scan(&n))
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

	// Both fresh fleets start due: leased passes run once across the merchant
	// set, while fleet-local passes run independently.
	_, stopOne := start(schemas[0], sets[0])
	_, stopTwo := start(schemas[1], sets[1])
	eventually("each leased pass is queued", func() bool {
		for _, kind := range leased {
			q := queued(kind)
			if (shared && q[schemas[0]]+q[schemas[1]] < 1) || (!shared && (q[schemas[0]] < 1 || q[schemas[1]] < 1)) {
				return false
			}
		}
		q := queued(perFleet[0])
		return q[schemas[0]] == 1 && q[schemas[1]] == 1
	})
	time.Sleep(2 * time.Second)
	for _, kind := range leased {
		q := queued(kind)
		if !shared {
			require.Equal(t, map[string]int{schemas[0]: 1, schemas[1]: 1}, q, "%s is queued by each fleet", kind)
			continue
		}
		require.Equal(t, 1, q[schemas[0]]+q[schemas[1]], "%s is queued once across both fleets: %v", kind, q)
		var holder string
		require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT holder FROM "+leases+" WHERE kind = $1", kind).Scan(&holder))
		require.Equal(t, 1, q[holder], "%s is queued by its lease holder", kind)
	}
	if !shared {
		return
	}

	// The holder stops. Age the survivor's retained checkpoints and expire the
	// old lease, then verify overdue work is taken over immediately.
	stopOne()
	stopTwo()
	before := map[string]int{}
	for _, kind := range leased {
		before[kind] = queued(kind)[schemas[1]]
	}
	agePeriodicHistory(t, f.pool, schemas[1], leased)
	_, err := f.pool.Exec(t.Context(), "UPDATE "+leases+" SET holder = $1, expires_at = now() - interval '1 second'", schemas[0])
	require.NoError(t, err)
	_, stopThree := start(schemas[1], sets[1])
	defer stopThree()
	eventually("the survivor takes the passes over", func() bool {
		for _, kind := range leased {
			if queued(kind)[schemas[1]] != before[kind]+1 {
				return false
			}
		}
		return true
	})
}
