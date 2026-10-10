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

// riverClock is River's clock moved by offset: a test stands just before a
// period boundary and time still passes.
type riverClock struct{ offset time.Duration }

func (c riverClock) Now() time.Time       { return time.Now().Add(c.offset) }
func (c riverClock) NowOrNil() *time.Time { now := c.Now(); return &now }

// Two replicas share one schedule. The leader stops just before the hour and
// the other replica takes over: every periodic sweep still runs at the hour,
// as the first leader would have run it, and the fleet queues each sweep once
// per period. A schedule counted from the takeover would slip by up to a full
// period at every change of leader, and never run while leaders change faster.
func TestPeriodicWorkKeepsItsBoundariesAcrossLeaders(t *testing.T) {
	f := newFixture(t)
	jobs := pgx.Identifier{f.schema, "river_job"}.Sanitize()
	leaders := pgx.Identifier{f.schema, "river_leader"}.Sanitize()

	// River runs ahead of the database's clock, so jobs SQL schedules at now()
	// are due at once.
	now := time.Now()
	hour := now.Truncate(time.Hour).Add(time.Hour)
	if hour.Sub(now) < 30*time.Second {
		hour = hour.Add(time.Hour)
	}
	clock := riverClock{offset: hour.Add(-12 * time.Second).Sub(now)}

	start := func(id string) (stop func()) {
		rt, err := openrails.New(t.Context(), f.config(), openrails.Deps{Postgres: f.pool})
		require.NoError(t, err)
		fleet, err := riverkit.New(t.Context(), f.pool, &river.Config{
			ID: id, Schema: f.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
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

	stopFirst := start("first")
	leads("first")
	start("second")
	stopFirst()
	leads("second")

	byPeriod := map[string]time.Duration{
		riverjobs.RebillWatchArgs{}.Kind():               riverjobs.RebillWatchInterval,
		riverjobs.AttemptEnrichmentArgs{}.Kind():         riverjobs.AttemptEnrichmentInterval,
		riverjobs.NMIHistoryArgs{}.Kind():                riverjobs.NMIHistoryInterval,
		riverjobs.PriceMigrationRedriveArgs{}.Kind():     time.Hour,
		riverjobs.CleanupExpiredDataArgs{}.Kind():        time.Hour,
		riverjobs.IdempotencyGCArgs{}.Kind():             15 * time.Minute,
		riverjobs.SolanaCrankArgs{}.Kind():               time.Hour,
		riverjobs.SolanaPayGCArgs{}.Kind():               15 * time.Minute,
		riverjobs.CreditExpiryArgs{}.Kind():              time.Hour,
		riverjobs.OrderExpiryArgs{}.Kind():               5 * time.Minute,
		riverjobs.AdmissionDenialFlushArgs{}.Kind():      5 * time.Minute,
		riverjobs.CatalogReconciliationPullArgs{}.Kind(): time.Hour,
		riverjobs.StripeWebhookReconcileArgs{}.Kind():    time.Hour,
		riverjobs.MerchantSecretCleanupArgs{}.Kind():     5 * time.Minute,
		riverjobs.DelinquencyArgs{}.Kind():               15 * time.Minute,
		riverjobs.ConvergeSweepArgs{}.Kind():             15 * time.Minute,
		riverjobs.NotificationEmailSweepArgs{}.Kind():    10 * time.Minute,
	}
	queued := func() map[string][]time.Time {
		rows, err := f.pool.Query(t.Context(), "SELECT kind, scheduled_at FROM "+jobs)
		require.NoError(t, err)
		out := map[string][]time.Time{}
		for rows.Next() {
			var kind string
			var at time.Time
			require.NoError(t, rows.Scan(&kind, &at))
			out[kind] = append(out[kind], at)
		}
		require.NoError(t, rows.Err())
		return out
	}
	atHour := func(at map[string][]time.Time) bool {
		for kind := range byPeriod {
			if !fromHour(at[kind], hour) {
				return false
			}
		}
		return true
	}
	at := queued()
	for deadline := time.Now().Add(30 * time.Second); !atHour(at) && time.Now().Before(deadline); at = queued() {
		time.Sleep(50 * time.Millisecond)
	}
	for kind, period := range byPeriod {
		require.True(t, fromHour(at[kind], hour), "the new leader runs %s at the hour; it queued it at %v", kind, at[kind])
		buckets := map[time.Time]int{}
		for _, scheduled := range at[kind] {
			buckets[scheduled.Truncate(period)]++
		}
		for bucket, n := range buckets {
			require.Equal(t, 1, n, "%s is queued once for the period from %s", kind, bucket.UTC())
		}
	}
}

// fromHour reports a job queued in the hour's first seconds: River records a
// run its schedule fired a moment late at its insertion.
func fromHour(times []time.Time, hour time.Time) bool {
	for _, at := range times {
		if !at.Before(hour) && at.Before(hour.Add(5*time.Second)) {
			return true
		}
	}
	return false
}
