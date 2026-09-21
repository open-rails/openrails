//go:build integration

package embed_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/embed"
)

func TestApplyMigrationsConcurrentFreshDatabase(t *testing.T) {
	admin := migrationConcurrencyAdmin(t)
	for _, schema := range []string{"", "shop_billing"} {
		name := schema
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			const callers = 8
			pool := migrationConcurrencyPool(t, admin, 2*callers+1)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			// Hold catalog writes until every initializer is waiting in PostgreSQL.
			// An unguarded CREATE SCHEMA checks for absence before waiting here,
			// so releasing the lock exposes its duplicate-key race deterministically.
			// A serialized initializer instead leaves the other callers waiting on
			// its migration lock. No migration-library lock key is assumed here.
			blocker, err := pool.Begin(ctx)
			require.NoError(t, err)
			defer func() { _ = blocker.Rollback(context.Background()) }()
			_, err = blocker.Exec(ctx, "LOCK TABLE pg_catalog.pg_namespace IN SHARE MODE")
			require.NoError(t, err)

			results := make(chan error, callers)
			start := make(chan struct{})
			opts := embed.MigrationOptions{Schema: schema, River: embed.RiverFromHost()}
			for range callers {
				go func() {
					<-start
					results <- embed.ApplyMigrations(ctx, pool, opts)
				}()
			}
			close(start)
			require.Eventually(t, func() bool {
				var waiting int
				err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'
					AND application_name = 'openrails_migration_concurrency'`).Scan(&waiting)
				return err == nil && waiting == callers
			}, 15*time.Second, 10*time.Millisecond, "all initializers must reach the database barrier")
			require.NoError(t, blocker.Commit(ctx))
			for range callers {
				select {
				case err := <-results:
					if err != nil {
						t.Errorf("concurrent initializer failed: %v", err)
					}
				case <-ctx.Done():
					t.Fatal("concurrent initialization did not finish:", ctx.Err())
				}
			}
			if t.Failed() {
				t.FailNow()
			}
			assertMigrationConcurrencyCatalog(t, ctx, pool, schema, false)
			assertMigrationConcurrencyPool(t, ctx, pool)
		})
	}
}

func TestApplyMigrationsSingleConnectionPool(t *testing.T) {
	admin := migrationConcurrencyAdmin(t)
	for _, hostRiver := range []bool{true, false} {
		name := "managed_river"
		if hostRiver {
			name = "host_river"
		}
		t.Run(name, func(t *testing.T) {
			pool := migrationConcurrencyPool(t, admin, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			var originalPID int32
			require.NoError(t, pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&originalPID))
			opts := embed.MigrationOptions{}
			if hostRiver {
				opts.River = embed.RiverFromHost()
			}
			require.NoError(t, embed.ApplyMigrations(ctx, pool, opts), "initializer must progress with one host connection")
			var ledgerBefore string
			require.NoError(t, pool.QueryRow(ctx, "SELECT jsonb_agg(m ORDER BY id)::text FROM public.migrations m").Scan(&ledgerBefore))
			require.NoError(t, embed.ApplyMigrations(ctx, pool, opts), "initialization must remain idempotent")
			var ledgerAfter string
			require.NoError(t, pool.QueryRow(ctx, "SELECT jsonb_agg(m ORDER BY id)::text FROM public.migrations m").Scan(&ledgerAfter))
			require.Equal(t, ledgerBefore, ledgerAfter, "reinitialization must preserve migration records")
			assertMigrationConcurrencyCatalog(t, ctx, pool, "", !hostRiver)
			assertMigrationConcurrencyPool(t, ctx, pool)
			var finalPID int32
			require.NoError(t, pool.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&finalPID))
			require.Equal(t, originalPID, finalPID, "the initializer must retain the host-owned session")
			require.EqualValues(t, 1, pool.Config().MaxConns)
		})
	}
}

// These tests cannot use dbtest's already-migrated shared fixture: it masks the
// fresh-schema race. Only databases created here are touched or dropped.
func migrationConcurrencyAdmin(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		dsn = os.Getenv("OPENRAILS_TEST_DB_DSN")
	}
	if dsn == "" {
		container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image: "postgres:18-alpine", ExposedPorts: []string{"5432/tcp"},
				Env:        map[string]string{"POSTGRES_USER": "postgres", "POSTGRES_PASSWORD": "test", "POSTGRES_DB": "postgres"},
				WaitingFor: wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			}, Started: true,
		})
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			require.NoError(t, container.Terminate(ctx))
		})
		host, err := container.Host(ctx)
		require.NoError(t, err)
		port, err := container.MappedPort(ctx, "5432/tcp")
		require.NoError(t, err)
		dsn = fmt.Sprintf("postgres://postgres:test@%s:%s/postgres?sslmode=disable", host, port.Port())
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.Database = "postgres"
	admin, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	require.NoError(t, admin.Ping(ctx))
	return admin
}

func migrationConcurrencyPool(t *testing.T, admin *pgxpool.Pool, maxConns int32) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	database := fmt.Sprintf("migration_concurrency_%d_%d", os.Getpid(), time.Now().UnixNano())
	_, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize()+" TEMPLATE template0")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err)
	})
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = database
	cfg.ConnConfig.RuntimeParams["application_name"] = "openrails_migration_concurrency"
	cfg.ConnConfig.RuntimeParams["search_path"] = "public"
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var objects int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_namespace
		WHERE nspname IN ('billing', 'shop_billing', 'openrails', 'profiles')`).Scan(&objects))
	require.Zero(t, objects, "fixture must start without application schemas")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_extension WHERE extname IN ('pgcrypto', 'btree_gist')`).Scan(&objects))
	require.Zero(t, objects, "fixture must start without application extensions")
	return pool
}

func assertMigrationConcurrencyCatalog(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string, managedRiver bool) {
	t.Helper()
	if schema == "" {
		schema = "billing"
	}
	for _, table := range []string{"merchants", "subscriptions", "invoices"} {
		var schemas []string
		require.NoError(t, pool.QueryRow(ctx, `SELECT array_agg(table_schema ORDER BY table_schema)
			FROM information_schema.tables WHERE table_name = $1`, table).Scan(&schemas))
		require.Equal(t, []string{schema}, schemas, "billing tables must exist once in the selected namespace")
	}
	var extensions int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM pg_extension WHERE extname IN ('pgcrypto', 'btree_gist')`).Scan(&extensions))
	require.Equal(t, 2, extensions)
	var records, distinctRecords int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), count(DISTINCT sequence)
		FROM public.migrations WHERE app = 'openrails' AND schema = $1 AND status = 'applied'`, schema).Scan(&records, &distinctRecords))
	require.Positive(t, records)
	require.Equal(t, records, distinctRecords, "each migration must be recorded exactly once")
	var riverExists bool
	require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass('public.river_job') IS NOT NULL").Scan(&riverExists))
	require.Equal(t, managedRiver, riverExists)
}

func assertMigrationConcurrencyPool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	require.NoError(t, pool.Ping(ctx), "the caller must retain a usable pool")
	var application, searchPath string
	require.NoError(t, pool.QueryRow(ctx, "SELECT current_setting('application_name'), current_setting('search_path')").Scan(&application, &searchPath))
	require.Equal(t, "openrails_migration_concurrency", application)
	require.Equal(t, "public", searchPath, "migration settings must not leak to host sessions")
	require.Zero(t, pool.Stat().AcquiredConns(), "initialization must return every borrowed connection")
}
