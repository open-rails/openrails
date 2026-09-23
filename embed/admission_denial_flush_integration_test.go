//go:build integration

package embed_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/redis/go-redis/v9"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// Exercise the real registration boundary: the optional runtime Redis pointer
// previously became a non-nil interface containing a nil *redis.Client.
func TestAdmissionDenialFlush_HostRiverOptionalRedis(t *testing.T) {
	for _, mode := range []string{"absent", "configured", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			schema := compositionRiverSchema(t)
			dsn := dbtest.SharedPostgresDSN(t)
			pool, err := pgxpool.New(ctx, dsn)
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			dbtest.EnsureTestMerchant(ctx, t, pool)
			var rdb *redis.Client
			if mode != "absent" {
				// Keep SCAN away from the default database used by other fixtures.
				// The configured case reserves this otherwise-empty test database;
				// cleanup deletes only this test's lock and counter key.
				rdb = redis.NewClient(&redis.Options{Addr: dbtest.SharedRedisAddr(t), DB: 15})
				t.Cleanup(func() { _ = rdb.Close() })
			}
			if mode == "configured" {
				const lock = "openrails:test:admission-denial-flush"
				owned, err := rdb.SetNX(ctx, lock, "reserved", 0).Result()
				require.NoError(t, err)
				require.True(t, owned, "another denial-flush test owns Redis DB 15")
				t.Cleanup(func() { require.NoError(t, rdb.Del(context.Background(), lock).Err()) })
				keys, err := rdb.Keys(ctx, admission.DenialKeyPrefix+"*").Result()
				require.NoError(t, err)
				require.Empty(t, keys, "refuse to flush another test's denial counters")
			}
			rt, err := embed.New(ctx, embed.Options{
				Config: &config.Config{TestMode: config.CredentialPostureSandbox,
					MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
					DB: &config.DBConfig{URL: dsn}},
				PGXPool: pool, Redis: rdb, River: embed.RiverFromHost(),
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
			jobs, err := riverhelpers.New(ctx, pool, &river.Config{Schema: schema,
				Queues: map[string]river.QueueConfig{embed.QueueBilling: {MaxWorkers: 1}}}, rt.RiverJobs())
			require.NoError(t, err)
			customer := uuid.New()
			now := time.Now().UTC()
			if mode == "configured" {
				recorder := admission.NewDenialRecorder(rdb)
				recorder.Record(ctx, dbtest.TestMerchantID.String(), customer.String(), "budget_exceeded", now)
				recorder.Record(ctx, dbtest.TestMerchantID.String(), customer.String(), "budget_exceeded", now)
				key := admission.DenialKey(dbtest.TestMerchantID.String(), now)
				t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })
			} else if mode == "unavailable" {
				require.NoError(t, rdb.Close())
			}
			events, cancel := jobs.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
			defer cancel()
			inserted, err := jobs.Insert(ctx, riverjobs.AdmissionDenialFlushArgs{}, &river.InsertOpts{Queue: embed.QueueBilling, MaxAttempts: 1})
			require.NoError(t, err)
			require.NoError(t, jobs.Start(ctx))
			t.Cleanup(func() { require.NoError(t, jobs.Stop(context.Background())) })
			deadline := time.NewTimer(30 * time.Second)
			defer deadline.Stop()
			for {
				select {
				case event := <-events:
					if event.Job.ID != inserted.Job.ID {
						continue
					}
					if mode == "unavailable" {
						require.Equal(t, river.EventKindJobFailed, event.Kind)
						require.Len(t, event.Job.Errors, 1)
						require.Contains(t, event.Job.Errors[0].Error, "client is closed")
						return
					}
					require.Equal(t, river.EventKindJobCompleted, event.Kind, "%+v", event.Job.Errors)
					require.Empty(t, event.Job.Errors)
					if mode == "configured" {
						ledger := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
						var denials int64
						require.NoError(t, ledger.QueryRow(ctx, `SELECT denials FROM billing.admission_denials_hourly WHERE customer_id=$1 AND denial_reason='budget_exceeded'`, customer).Scan(&denials))
						require.Equal(t, int64(2), denials)
						remaining, err := rdb.HGet(ctx, admission.DenialKey(dbtest.TestMerchantID.String(), now), customer.String()+"|budget_exceeded").Int64()
						require.NoError(t, err)
						require.Zero(t, remaining)
					}
					return
				case <-deadline.C:
					t.Fatal("registered admission denial flush did not finish")
				}
			}
		})
	}
}
