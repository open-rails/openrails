//go:build integration

package embed_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/pgidentity"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/require"
)

func TestPhysicalRiverPoolBinding(t *testing.T) {
	for _, mode := range []string{"same pool one connection", "distinct role and alias same database", "different database same canonical queue", "missing queue schema", "ambient transaction", "ambient merchant pin", "caller search path differs"} {
		t.Run(mode, func(t *testing.T) {
			ctx := dbtest.WithTestMerchant(t.Context())
			dsn := dbtest.SharedPostgresDSN(t)
			cfg, err := pgxpool.ParseConfig(dsn)
			require.NoError(t, err)
			cfg.MaxConns = 1
			if mode == "caller search path differs" {
				cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
			}
			ledgerPool, err := pgxpool.NewWithConfig(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(ledgerPool.Close)
			rt, err := embed.New(ctx, embed.Options{Config: &config.Config{TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn}, Auth: &config.AuthConfig{Issuer: "https://physical-binding.test", KeysPath: t.TempDir()}}, PGXPool: ledgerPool, River: embed.RiverFromHost()})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
			queuePool := ledgerPool
			schema := "public"
			if mode == "distinct role and alias same database" || mode == "different database same canonical queue" {
				cfg, err := pgxpool.ParseConfig(dbtest.SharedSuperuserDSN(t))
				require.NoError(t, err)
				cfg.MaxConns = 1
				if mode == "distinct role and alias same database" {
					cfg.ConnConfig.Host = "localhost"
				}
				admin, err := pgxpool.NewWithConfig(ctx, cfg)
				require.NoError(t, err)
				t.Cleanup(admin.Close)
				queuePool = admin
				if mode == "different database same canonical queue" {
					name := "binding_" + strings.ReplaceAll(uuid.NewString(), "-", "")
					_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
					require.NoError(t, err)
					t.Cleanup(func() {
						_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
						require.NoError(t, err)
					})
					otherCfg := cfg.Copy()
					otherCfg.ConnConfig.Database = name
					queuePool, err = pgxpool.NewWithConfig(ctx, otherCfg)
					require.NoError(t, err)
					t.Cleanup(queuePool.Close)
					migrations, err := rivermigrate.New(riverpgxv5.New(queuePool), &rivermigrate.Config{Schema: schema})
					require.NoError(t, err)
					_, err = migrations.Migrate(ctx, rivermigrate.DirectionUp, nil)
					require.NoError(t, err)
				}
			}
			if mode == "missing queue schema" {
				schema = "absent_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			}
			d := app.HostGraph(rt).Runtime.DB
			var client *river.Client[pgx.Tx]
			bind := func(ctx context.Context) error {
				var err error
				options := &river.Config{Schema: schema}
				if mode == "caller search path differs" {
					options = nil
				}
				client, err = riverhelpers.New(ctx, queuePool, options, rt.RiverJobs())
				return err
			}
			if mode == "ambient transaction" {
				err = d.MerchantTx(ctx, func(ctx context.Context, _ pgx.Tx) error { return bind(ctx) })
			} else if mode == "ambient merchant pin" {
				err = d.RunInMerchantConn(ctx, func(ctx context.Context) error {
					var one int
					require.NoError(t, d.Qx(ctx).QueryRow(ctx, "SELECT 1").Scan(&one))
					return bind(ctx)
				})
			} else {
				err = bind(ctx)
			}
			refused := mode == "different database same canonical queue" || mode == "missing queue schema" || strings.HasPrefix(mode, "ambient")
			if refused {
				require.Error(t, err)
				require.Nil(t, client)
				require.False(t, rt.HasExternalRiverClient())
				if mode == "different database same canonical queue" {
					require.ErrorIs(t, err, pgidentity.ErrDifferentDatabase)
				} else if strings.HasPrefix(mode, "ambient") {
					require.ErrorIs(t, err, db.ErrCallerTransaction)
				} else {
					require.ErrorContains(t, err, "queue table is missing")
				}
			} else {
				require.NoError(t, err)
				require.True(t, rt.HasExternalRiverClient())
			}
			mid := dbtest.TestMerchantID.UUID()
			psp := dbtest.EnsureTestPSP(ctx, t, ledgerPool, mid, "nmi")
			key := uuid.NewString()
			params := intents.EnqueueParams{MerchantID: mid, Provider: "nmi", PspID: psp, IntentType: "test_binding", IdempotencyKey: key, Origin: intents.OriginSystem, NextAttemptAt: time.Now()}
			if refused {
				_, err := intents.NewStore(d).Enqueue(ctx, params)
				require.ErrorContains(t, err, "bound River producer")
			} else {
				rollback := errors.New("host rollback")
				err := d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					_, err := intents.NewStore(d.NewWithPgxTx(tx)).Enqueue(ctx, params)
					require.NoError(t, err, "ambient max-one-connection transaction must not borrow its pool during insertion")
					return rollback
				})
				require.ErrorIs(t, err, rollback)
			}
			var count int
			require.NoError(t, ledgerPool.QueryRow(ctx, "SELECT count(*) FROM billing.rail_intents WHERE idempotency_key=$1", key).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, ledgerPool.Ping(ctx))
			require.NoError(t, queuePool.Ping(ctx), "library must not close host pool")
		})
	}
}

func TestMigrationRuntimePoolPhysicalIdentityBeforeDDL(t *testing.T) {
	for _, sameName := range []bool{false, true} {
		name := "different database same cluster"
		if sameName {
			name = "same database name different cluster"
		}
		t.Run(name, func(t *testing.T) {
			firstDSN := dbtest.SharedSuperuserDSN(t)
			secondDSN := firstDSN
			if sameName {
				secondDSN = os.Getenv("OPENRAILS_IDENTITY_OTHER_CLUSTER_DSN")
				if secondDSN == "" {
					secondDSN = dbtest.IsolatedPostgresDSN(t)
				}
			}
			database := "migration_identity_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			var pools []*pgxpool.Pool
			for i, dsn := range []string{firstDSN, secondDSN} {
				admin, err := pgxpool.New(t.Context(), dsn)
				require.NoError(t, err)
				t.Cleanup(admin.Close)
				dbName := database
				if i == 1 && !sameName {
					dbName += "_other"
				}
				_, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
					require.NoError(t, err)
				})
				cfg := admin.Config().Copy()
				cfg.ConnConfig.Database = dbName
				cfg.MaxConns = 1
				pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
				require.NoError(t, err)
				t.Cleanup(pool.Close)
				pools = append(pools, pool)
			}
			err := embed.ApplyMigrations(t.Context(), pools[0], embed.MigrationOptions{RuntimePool: pools[1]})
			require.ErrorIs(t, err, pgidentity.ErrDifferentDatabase)
			var absent bool
			require.NoError(t, pools[0].QueryRow(t.Context(), "SELECT pg_catalog.to_regnamespace('billing') IS NULL").Scan(&absent))
			require.True(t, absent, "runtime pool mismatch must fail before schema mutation")
		})
	}
}

type physicalBindingArgs struct {
	ID string `json:"id"`
}

func (physicalBindingArgs) Kind() string { return "test.physical_binding" }

type physicalBindingWorker struct {
	river.WorkerDefaults[physicalBindingArgs]
	seen chan string
}

func (w *physicalBindingWorker) Work(_ context.Context, job *river.Job[physicalBindingArgs]) error {
	w.seen <- job.Args.ID
	return nil
}

func TestPhysicalBindingWorkerAndInsertTxShareExplicitSchema(t *testing.T) {
	for _, schema := range []string{"public", "openrails", "binding_jobs_" + uuid.NewString()[:8]} {
		t.Run(schema, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(dbtest.WithTestMerchant(t.Context()), 30*time.Second)
			defer cancel()
			runtimeCfg, err := pgxpool.ParseConfig(dbtest.SharedPostgresDSN(t))
			require.NoError(t, err)
			runtimeCfg.MaxConns = 1
			runtimeCfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog"
			ledgerPool, err := pgxpool.NewWithConfig(ctx, runtimeCfg)
			require.NoError(t, err)
			t.Cleanup(ledgerPool.Close)
			queueCfg, err := pgxpool.ParseConfig(dbtest.SharedSuperuserDSN(t))
			require.NoError(t, err)
			queueCfg.MaxConns = 1
			queueCfg.ConnConfig.RuntimeParams["search_path"] = "public"
			queuePool, err := pgxpool.NewWithConfig(ctx, queueCfg)
			require.NoError(t, err)
			t.Cleanup(queuePool.Close)
			_, err = queuePool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize())
			require.NoError(t, err)
			migration, err := rivermigrate.New(riverpgxv5.New(queuePool), &rivermigrate.Config{Schema: schema})
			require.NoError(t, err)
			_, err = migration.Migrate(ctx, rivermigrate.DirectionUp, nil)
			require.NoError(t, err)
			var role string
			require.NoError(t, ledgerPool.QueryRow(ctx, "SELECT current_user").Scan(&role))
			_, err = queuePool.Exec(ctx, "GRANT USAGE ON SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			_, err = queuePool.Exec(ctx, "GRANT ALL ON ALL TABLES IN SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			_, err = queuePool.Exec(ctx, "GRANT ALL ON ALL SEQUENCES IN SCHEMA "+pgx.Identifier{schema}.Sanitize()+" TO "+pgx.Identifier{role}.Sanitize())
			require.NoError(t, err)
			d, err := db.NewWithPGXPool(ledgerPool, config.DefaultSchema)
			require.NoError(t, err)
			seen := make(chan string, 1)
			jobs := riverhelpers.NewContribution("physical-proof", func(_ context.Context, cfg *river.Config) error {
				cfg.Queues["physical_proof"] = river.QueueConfig{MaxWorkers: 1}
				return river.AddWorkerSafely(cfg.Workers, &physicalBindingWorker{seen: seen})
			}, func(ctx context.Context, binding riverhelpers.Binding) error {
				require.Same(t, queuePool, binding.Pool)
				if err := d.ValidateRiverJobBinding(ctx, binding.Pool, binding.Client.Schema()); err != nil {
					return err
				}
				d.SetRiverJobInserter(binding.Client)
				return nil
			}, func() error { d.SetRiverJobInserter(nil); return nil })
			configuredSchema := schema
			if schema == "public" {
				configuredSchema = ""
			} // The shared River composer must pin its public default.
			client, err := riverhelpers.New(ctx, queuePool, &river.Config{Schema: configuredSchema, FetchCooldown: 10 * time.Millisecond, FetchPollInterval: 20 * time.Millisecond}, jobs)
			require.NoError(t, err)
			require.Equal(t, schema, client.Schema())
			id := uuid.NewString()
			err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if err := d.InsertRiverJobTx(ctx, tx, physicalBindingArgs{ID: id}, &river.InsertOpts{Queue: "physical_proof"}); err != nil {
					return err
				}
				var visible int
				require.NoError(t, queuePool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE args->>'id'=$1", id).Scan(&visible))
				require.Zero(t, visible, "worker connection cannot observe an uncommitted job")
				return nil
			})
			require.NoError(t, err)
			require.NoError(t, client.Start(ctx))
			select {
			case got := <-seen:
				require.Equal(t, id, got)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.NoError(t, client.Stop(context.Background()))
			var state string
			require.NoError(t, queuePool.QueryRow(ctx, "SELECT state FROM "+pgx.Identifier{schema, "river_job"}.Sanitize()+" WHERE args->>'id'=$1", id).Scan(&state))
			require.Equal(t, "completed", state)
			require.NoError(t, ledgerPool.Ping(ctx))
			require.NoError(t, queuePool.Ping(ctx))
		})
	}
}
