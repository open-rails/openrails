//go:build integration

// Package dbtest provides a shared Postgres database for integration tests.
//
// SharedPostgresDSN spins up a single postgres:18-alpine testcontainer once per
// test-package process (via sync.Once), bootstraps the billing/profiles schemas,
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
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/lib/pq" // database/sql driver used for schema bootstrap
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/migrate"
)

var (
	sharedOnce sync.Once
	sharedDSN  string
	sharedErr  error
)

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
	if dsn == "" {
		container, err := postgres.Run(ctx,
			"postgres:18-alpine",
			postgres.WithDatabase("test_db"),
			postgres.WithUsername("test_user"),
			postgres.WithPassword("test_password"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).
					WithStartupTimeout(60*time.Second),
			),
		)
		if err != nil {
			return "", fmt.Errorf("start postgres container: %w", err)
		}
		// The container is intentionally not terminated here: it must outlive
		// every test in the package. testcontainers' Ryuk reaper removes it when
		// the test session ends.
		dsn, err = container.ConnectionString(ctx, "sslmode=disable")
		if err != nil {
			return "", fmt.Errorf("postgres connection string: %w", err)
		}
	}

	if err := bootstrapAndMigrate(ctx, dsn); err != nil {
		return "", err
	}
	return dsn, nil
}

func bootstrapAndMigrate(ctx context.Context, dsn string) error {
	sqlDB, err := sql.Open("postgres", dsn)
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
	// the billing schema + pgcrypto, then the authkit migrations create the
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
