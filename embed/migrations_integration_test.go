//go:build integration

package embed

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
	"golang.org/x/sync/errgroup"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// Fresh databases prove the consumer entrypoint owns its catalog and grants only
// the River objects it manages, including objects introduced by the pinned River
// release. A pre-migrated shared fixture would hide unwanted default grants.
func TestApplyMigrationsFreshOwnershipAndSchemas(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		dsn = os.Getenv("OPENRAILS_TEST_DB_DSN")
	}
	if dsn == "" {
		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{Image: "postgres:18-alpine", ExposedPorts: []string{"5432/tcp"}, Env: map[string]string{"POSTGRES_USER": "postgres", "POSTGRES_PASSWORD": "test", "POSTGRES_DB": "postgres"}, WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2)}, Started: true,
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, container.Terminate(context.Background())) })
		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432/tcp")
		require.NoError(t, err)
		dsn = fmt.Sprintf("postgres://postgres:test@%s:%s/postgres?sslmode=disable", host, port.Port())
	}
	adminConfig, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	adminConfig.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(ctx, adminConfig)
	require.NoError(t, err)
	t.Cleanup(admin.Close)

	for _, tc := range []struct {
		name, billing, jobs string
		host                bool
	}{
		{name: "defaults", billing: "billing", jobs: "public"},
		{name: "custom", billing: "store_billing", jobs: "store_jobs"},
		{name: "canonical", billing: "openrails", jobs: "jobs"},
		{name: "host_owned", billing: "billing", jobs: "host_jobs", host: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runtimeUser := fmt.Sprintf("Host \"Runtime\" %d", time.Now().UnixNano())
			_, err := admin.Exec(ctx, "CREATE ROLE "+pgx.Identifier{runtimeUser}.Sanitize()+" LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD 'runtime_test'")
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := admin.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{runtimeUser}.Sanitize())
				require.NoError(t, err)
			})
			database := fmt.Sprintf("migration_contract_%d", time.Now().UnixNano())
			_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
			require.NoError(t, err)
			t.Cleanup(func() {
				_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
				require.NoError(t, err)
			})
			poolConfig := adminConfig.Copy()
			poolConfig.ConnConfig.Database = database
			pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			_, err = pool.Exec(ctx, `CREATE TABLE public.host_before (id bigserial); CREATE TABLE public.river_host_app (id bigserial)`)
			require.NoError(t, err)
			var hostClient *river.Client[pgx.Tx]
			binderCalled := false
			opts := MigrationOptions{}
			if tc.name != "defaults" {
				opts.Schema = tc.billing
				opts.River = RiverManagedByOpenRails(tc.jobs)
			}
			if tc.host {
				_, err = pool.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{tc.jobs}.Sanitize())
				require.NoError(t, err)
				migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: tc.jobs})
				require.NoError(t, err)
				_, err = migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
				require.NoError(t, err)
				opts.River = RiverFromHost()
			}
			if tc.name == "defaults" {
				mismatched := opts
				mismatched.RuntimePool = admin
				require.ErrorContains(t, ApplyMigrations(ctx, pool, mismatched), "differs from migration database")
				var exists bool
				require.NoError(t, pool.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", tc.billing).Scan(&exists))
				require.False(t, exists, "runtime preflight must happen before schema DDL")
			}
			require.NoError(t, ApplyMigrations(ctx, pool, opts))
			var granted bool
			require.NoError(t, pool.QueryRow(ctx, "SELECT has_table_privilege($1,$2,'SELECT')", runtimeUser, tc.billing+".merchants").Scan(&granted))
			require.False(t, granted, "migration-only initialization must not guess a runtime role")
			runtimeConfig := poolConfig.Copy()
			runtimeConfig.ConnConfig.User = runtimeUser
			runtimeConfig.ConnConfig.Password = "runtime_test"
			runtimePool, err := pgxpool.NewWithConfig(ctx, runtimeConfig)
			require.NoError(t, err)
			t.Cleanup(runtimePool.Close)
			opts.RuntimePool = runtimePool
			require.NoError(t, ApplyMigrations(ctx, pool, opts), "repeated initialization is idempotent")
			var concurrent errgroup.Group
			for range 4 {
				concurrent.Go(func() error { return ApplyMigrations(ctx, pool, opts) })
			}
			require.NoError(t, concurrent.Wait(), "concurrent runtime ACL provisioning must serialize")
			var canUpdate bool
			require.NoError(t, runtimePool.QueryRow(ctx, "SELECT has_table_privilege(current_user,$1,'UPDATE')", tc.billing+".ledger_accounts").Scan(&canUpdate))
			require.False(t, canUpdate, "runtime must not rewrite ledger counters")
			require.False(t, binderCalled, "migration must never construct the host client")
			_, err = pool.Exec(ctx, `CREATE TABLE public.host_after (id bigserial)`)
			require.NoError(t, err)
			for _, table := range []string{"public.host_before", "public.river_host_app", "public.host_after"} {
				var allowed bool
				require.NoError(t, pool.QueryRow(ctx, `SELECT has_table_privilege($2,$1,'SELECT,INSERT,UPDATE,DELETE')`, table, runtimeUser).Scan(&allowed))
				require.False(t, allowed, table)
				require.NoError(t, pool.QueryRow(ctx, `SELECT has_sequence_privilege($2,$1,'USAGE,SELECT,UPDATE')`, table+"_id_seq", runtimeUser).Scan(&allowed))
				require.False(t, allowed, table+" sequence")
			}
			var exists bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, tc.billing+".merchants").Scan(&exists))
			require.True(t, exists)
			require.NoError(t, pool.QueryRow(ctx, `SELECT to_regnamespace('profiles') IS NOT NULL`).Scan(&exists))
			require.False(t, exists, "AuthKit remains independently owned")
			require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, tc.billing+".river_job").Scan(&exists))
			require.False(t, exists)
			for _, table := range []string{"river_job", "river_queue", "river_leader", "river_notification"} {
				var allowed bool
				require.NoError(t, pool.QueryRow(ctx, `SELECT has_table_privilege($2,$1,'SELECT,INSERT,UPDATE,DELETE')`, tc.jobs+"."+table, runtimeUser).Scan(&allowed))
				require.Equal(t, !tc.host, allowed, table)
			}
			var allowed bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT has_table_privilege($2,$1,'SELECT,INSERT,UPDATE,DELETE')`, tc.jobs+".river_migration", runtimeUser).Scan(&allowed))
			require.False(t, allowed)
			if tc.jobs != "public" {
				require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('public.river_job') IS NOT NULL`).Scan(&exists))
				require.False(t, exists)
			}

			if tc.host {
				// The host chooses its own access policy. OpenRails did not grant these.
				_, err = pool.Exec(ctx, "GRANT USAGE ON SCHEMA "+pgx.Identifier{tc.jobs}.Sanitize()+" TO "+pgx.Identifier{runtimeUser}.Sanitize()+"; GRANT SELECT ON TABLE "+pgx.Identifier{tc.jobs, "river_job"}.Sanitize()+" TO "+pgx.Identifier{runtimeUser}.Sanitize())
				require.NoError(t, err)
			}
			runtimeURL, err := url.Parse(dsn)
			require.NoError(t, err)
			runtimeURL.Path = "/" + database
			runtimeURL.User = url.UserPassword(runtimeUser, "runtime_test")
			runtimeSchema := opts.Schema
			rt, err := New(ctx, Options{Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: runtimeURL.String(), Schema: runtimeSchema}}, PGXPool: runtimePool, River: opts.River})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
			require.False(t, binderCalled, "New must leave composition open")
			if tc.host {
				hostClient, err = rt.BindRiver(ctx, pool, func(_ context.Context, cfg *river.Config) error {
					binderCalled = true
					cfg.Schema = tc.jobs
					cfg.Queues[QueueBilling] = river.QueueConfig{MaxWorkers: 1}
					return nil
				})
				require.NoError(t, err)
			}
			require.Equal(t, tc.host, rt.HasExternalRiverClient())
			require.Equal(t, tc.billing, rt.app.Runtime.DB.DataPool().Schema())
			// A nested service reconstructing DB from a library transaction must
			// retain its schema, including canonical openrails (an identity rewrite).
			// Only one billing namespace exists in each fresh database, so a wrong
			// default cannot silently pass through a duplicate fixture schema.
			tx, err := rt.app.Runtime.DB.DataPool().Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(context.Background()) }()
			bound := db.NewWithPgxTx(tx)
			_, err = bound.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES(gen_random_uuid(),'transaction-schema-proof')`)
			require.NoError(t, err)
			var relation string
			require.NoError(t, bound.Qx(ctx).QueryRow(ctx, `SELECT tableoid::regclass::text FROM openrails.merchants WHERE slug='transaction-schema-proof'`).Scan(&relation))
			require.Equal(t, tc.billing+".merchants", relation)
			require.NoError(t, tx.Rollback(ctx))
			require.NoError(t, rt.app.Runtime.InitRiver(ctx))
			require.Equal(t, tc.jobs, rt.app.Runtime.RiverClient.Schema())
			require.Equal(t, tc.jobs, rt.app.Runtime.RiverProducer.Schema())
			_, err = rt.app.Runtime.RiverProducer.Insert(ctx, InvoiceSweepArgs{}, nil)
			require.NoError(t, err)
			var count int
			require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM "+pgx.Identifier{tc.jobs, "river_job"}.Sanitize()).Scan(&count))
			require.Equal(t, 1, count)
			_, err = rt.CheckJobProgress(ctx)
			require.NoError(t, err, "progress monitor uses the same River namespace")
			if tc.host {
				require.True(t, binderCalled)
				require.Same(t, hostClient, rt.app.Runtime.RiverClient)
				require.Nil(t, hostClient.Stopped(), "OpenRails must not start the host client")
				require.NoError(t, hostClient.Start(ctx))
				t.Cleanup(func() { require.NoError(t, hostClient.Stop(context.Background())) })
				require.NoError(t, rt.Close(ctx))
				select {
				case <-hostClient.Stopped():
					t.Fatal("OpenRails stopped the host client")
				default:
				}
				// Closing the library does not dispose of the host client or its pool.
				_, err = hostClient.Insert(ctx, InvoiceSweepArgs{}, nil)
				require.NoError(t, err)
			}
		})
	}
}
