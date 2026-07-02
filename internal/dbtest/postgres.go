//go:build integration

// Package dbtest provides a shared Postgres database for integration tests.
//
// SharedPostgresDSN spins up a single postgres:18-alpine testcontainer once per
// test-package process (via sync.Once), bootstraps the openrails/profiles schemas,
// applies all migrations via internal/migrate.RunPostgres, and hands the same DSN
// to every caller. Because the container is shared for the lifetime of the
// package's test binary, state persists across tests in that package — callers
// are responsible for any per-test isolation/cleanup they need (most scope rows
// by a freshly-generated owner id).
//
// If OPENRAILS_TEST_DB_URL or OPENRAILS_TEST_DB_DSN is set, that database is
// used instead of a testcontainer (e.g. a docker-compose Postgres stack).
// Migrations are still applied; migratekit/River migrations are idempotent, so
// this is safe to repeat.
package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/migrate"
)

var (
	sharedOnce      sync.Once
	sharedDSN       string
	sharedErr       error
	sharedContainer testcontainers.Container

	// External-DB mode: the per-run database this process created via
	// createExternalTestDatabase, and the admin DSN to drop it with on teardown.
	// (Testcontainer mode tears down the whole container instead.)
	sharedExternalAdminDSN string
	sharedExternalDBName   string
)

// testDBStaleAfter bounds how long an orphaned per-run test database (left by a
// crashed/killed process whose teardown never ran) may live before the next run
// reaps it. Generously above the per-package test timeout so a database in
// active use by a concurrent run is never mistaken for an orphan.
const testDBStaleAfter = 30 * time.Minute

// RunMain is the TestMain entry point for any package that uses
// SharedPostgresDSN. It runs the package's tests and then ALWAYS terminates the
// shared Postgres container, so the container is closed after use even when the
// testcontainers Ryuk reaper is unavailable (e.g. offline/sandboxed local runs
// where Ryuk's image can't be pulled or RYUK is disabled). Without this, the
// sync.Once container would leak for the lifetime of the Docker daemon.
//
// Usage, in each consuming package (behind the integration build tag):
//
//	func TestMain(m *testing.M) { dbtest.RunMain(m) }
func RunMain(m *testing.M) {
	code := m.Run()
	TerminateShared()
	TerminateSharedRedis()
	TerminateSharedClickHouse()
	os.Exit(code)
}

// TerminateShared terminates the shared Postgres container if this process
// started one. It is safe to call when no container was started (e.g. an
// external OPENRAILS_TEST_DB_URL was used) and idempotent on repeat calls.
func TerminateShared() {
	// External-DB mode: drop the per-run database we created so it does not leak
	// on the shared server (the data dir would otherwise bloat until postgres
	// startup-fsync times out and the container OOM-kills in a crash loop).
	if sharedExternalDBName != "" && sharedExternalAdminDSN != "" {
		dropExternalTestDatabase(sharedExternalAdminDSN, sharedExternalDBName)
		sharedExternalDBName, sharedExternalAdminDSN = "", ""
	}
	if sharedContainer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sharedContainer.Terminate(ctx); err != nil {
		fmt.Printf("dbtest: failed to terminate shared postgres container: %v\n", err)
	}
	sharedContainer = nil
}

// dropExternalTestDatabase force-drops a per-run test database on the shared
// server, terminating any lingering connections. Best-effort.
func dropExternalTestDatabase(adminDSN, dbName string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return
	}
	cfg.ConnConfig.Config.Database = "postgres"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
}

// reapStaleTestDatabases drops orphaned per-run test databases left by
// crashed/killed prior runs whose teardown never ran — both `openrails_it_*`
// (SharedPostgresDSN) and `openrails_bootstrap_*` (newExternalPostgresTestPool).
// Only databases older than testDBStaleAfter are reaped, so a database in active
// use by a concurrent run is never dropped. Best-effort: errors (incl. races
// with another reaper) are ignored.
func reapStaleTestDatabases(ctx context.Context, adminPool *pgxpool.Pool) {
	rows, err := adminPool.Query(ctx, `SELECT datname FROM pg_database WHERE datname LIKE 'openrails\_it\_%' OR datname LIKE 'openrails\_bootstrap\_%'`)
	if err != nil {
		return
	}
	var stale []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) != nil {
			continue
		}
		if testDBIsStale(name) {
			stale = append(stale, name)
		}
	}
	rows.Close()
	for _, name := range stale {
		_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}
}

// testDBIsStale parses the trailing `_<unixnano>` from a per-run test database
// name and reports whether it is older than testDBStaleAfter. Unparseable names
// are treated as not stale (never reaped).
func testDBIsStale(name string) bool {
	i := strings.LastIndex(name, "_")
	if i < 0 || i+1 >= len(name) {
		return false
	}
	nano, err := strconv.ParseInt(name[i+1:], 10, 64)
	if err != nil || nano <= 0 {
		return false
	}
	return time.Since(time.Unix(0, nano)) > testDBStaleAfter
}

// SharedPostgresDSN returns a DSN to a migrated Postgres shared across all
// integration tests in the calling package process. It fails the test if the
// database cannot be provisioned.
func SharedPostgresDSN(t *testing.T) string {
	t.Helper()
	sharedOnce.Do(func() {
		sharedDSN, sharedErr = provision(context.Background())
	})
	require.NoError(t, sharedErr, "provision shared integration postgres")
	return sharedDSN
}

func provision(ctx context.Context) (string, error) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN"))
	}
	if dsn != "" {
		isolatedDSN, err := createExternalTestDatabase(ctx, dsn)
		if err != nil {
			return "", err
		}
		dsn = isolatedDSN
	} else {
		container, err := testcontainers.GenericContainer(ctx,
			testcontainers.GenericContainerRequest{
				ContainerRequest: testcontainers.ContainerRequest{
					Image:        "postgres:18-alpine",
					ExposedPorts: []string{"5432/tcp"},
					Env: map[string]string{
						"POSTGRES_DB":       "test_db",
						"POSTGRES_USER":     "test_user",
						"POSTGRES_PASSWORD": "test_password",
					},
					WaitingFor: wait.ForLog("database system is ready to accept connections").
						WithOccurrence(2).
						WithStartupTimeout(60 * time.Second),
					HostConfigModifier: PostgresHostConfigModifier,
				},
				Started: true,
			},
		)
		if err != nil {
			return "", fmt.Errorf("start postgres container: %w", err)
		}
		// The container must outlive every test in the package, so it is NOT
		// terminated here. It is retained and torn down by TerminateShared, which
		// RunMain invokes from the package's TestMain after m.Run(). (testcontainers'
		// Ryuk reaper is a secondary safety net but cannot be relied upon in
		// offline/sandboxed runs where its image is unavailable.)
		sharedContainer = container
		host, err := container.Host(ctx)
		if err != nil {
			return "", fmt.Errorf("postgres container host: %w", err)
		}
		port, err := container.MappedPort(ctx, "5432/tcp")
		if err != nil {
			return "", fmt.Errorf("postgres mapped port: %w", err)
		}
		dsn = (&url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword("test_user", "test_password"),
			Host:     net.JoinHostPort(host, port.Port()),
			Path:     "/test_db",
			RawQuery: "sslmode=disable",
		}).String()
	}

	if err := bootstrapAndMigrate(ctx, dsn); err != nil {
		return "", err
	}
	return dsn, nil
}

func createExternalTestDatabase(ctx context.Context, adminDSN string) (string, error) {
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return "", fmt.Errorf("parse external postgres dsn: %w", err)
	}
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	if err != nil {
		return "", fmt.Errorf("connect external postgres admin database: %w", err)
	}
	defer adminPool.Close()

	// Reap orphaned per-run databases from crashed/killed prior runs before
	// adding another, so the shared server's data dir cannot grow without bound.
	reapStaleTestDatabases(ctx, adminPool)

	dbName := fmt.Sprintf("openrails_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize()); err != nil {
		return "", fmt.Errorf("create external test database %s: %w", dbName, err)
	}
	// Record so TerminateShared (invoked by RunMain's TestMain) drops it on exit.
	sharedExternalAdminDSN = adminDSN
	sharedExternalDBName = dbName

	// NOTE: pgxpool's Config.ConnString() returns the ORIGINAL string the
	// config was parsed from — mutating Config.Database does not change it.
	// The DSN must be rewritten textually, or every "isolated" test database
	// silently aliases the admin/dev database and the migrator runs against
	// it (which is how migrations 003/004 once got replayed onto a live dev
	// DB). Rewrite the URL path / dbname key explicitly instead.
	return replaceDSNDatabase(adminDSN, dbName)
}

// replaceDSNDatabase returns dsn with its database swapped to dbName,
// handling both URL DSNs (postgres://...) and key=value DSNs.
func replaceDSNDatabase(dsn, dbName string) (string, error) {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", fmt.Errorf("parse external postgres url: %w", err)
		}
		u.Path = "/" + dbName
		return u.String(), nil
	}
	// key=value form: drop any existing dbname and append the new one.
	fields := strings.Fields(dsn)
	out := make([]string, 0, len(fields)+1)
	for _, f := range fields {
		if strings.HasPrefix(f, "dbname=") {
			continue
		}
		out = append(out, f)
	}
	out = append(out, "dbname="+dbName)
	return strings.Join(out, " "), nil
}

func bootstrapAndMigrate(ctx context.Context, dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open postgres for bootstrap: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	// The postgres image logs "ready to accept connections" during its init
	// phase and again after restarting, and even after the wait strategy the
	// freshly-mapped port can briefly reset connections. Ping with backoff so a
	// transient startup race doesn't get cached as a hard failure by sync.Once.
	if err := waitForDB(ctx, sqlDB); err != nil {
		return fmt.Errorf("wait for postgres: %w", err)
	}

	// migrate.RunPostgres owns the full schema: ensurePostgresBootstrap creates
	// the configured OpenRails schema (default openrails) + pgcrypto, then the authkit migrations create the
	// profiles schema and all of its tables (users, *_roles, the role_id()
	// function, etc.). We deliberately do NOT pre-create any profiles tables —
	// doing so shadows authkit's own migration-managed definitions and breaks
	// later authkit migrations (e.g. missing users.phone_number).
	cfg := &config.Config{
		Env: "dev",
		DB:  &config.DBConfig{URL: dsn},
	}
	if err := migrate.RunPostgres(ctx, cfg); err != nil {
		return fmt.Errorf("run migrations: %w", err)
	}

	// Seed the canonical test merchant once per provisioned DB so every test has a
	// valid merchant FK target. Many integration tests insert merchant-scoped rows
	// (products, prices, subscriptions, payment_methods, ...) against TestMerchantID
	// without first calling EnsureTestMerchant; seeding it here makes that robust and
	// order-independent. Idempotent — EnsureTestMerchant remains a safe no-op.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active') ON CONFLICT (slug) DO NOTHING`,
		TestMerchantID.UUID(), TestMerchantSlug); err != nil {
		return fmt.Errorf("seed test merchant: %w", err)
	}
	return nil
}

// waitForDB pings the database until it responds or the deadline elapses.
func waitForDB(ctx context.Context, sqlDB *sql.DB) error {
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = sqlDB.PingContext(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database not ready after 60s: %w", lastErr)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// SharedPGXPool returns a pgx/v5 pool on the shared integration postgres —
// the handle for exercising sqlc-converted code paths (#334).
func SharedPGXPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), SharedPostgresDSN(t))
	require.NoError(t, err, "open pgx pool on shared integration postgres")
	t.Cleanup(pool.Close)
	return pool
}
