//go:build integration

package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/stretchr/testify/require"
)

const (
	// appRole is the unprivileged, NOBYPASSRLS role created by 0001_schema.up.sql that
	// production connects as so the per-merchant RLS policies actually constrain
	// queries. It is the DEFAULT role integration tests connect as.
	appRole = "openrails_app"
	// appPassword is attached to appRole for tests only (production wires the
	// credential out of band). The role keeps NOBYPASSRLS, so RLS still enforces.
	appPassword = "app_pw"
)

var (
	rlsOnce   sync.Once
	rlsAppDSN string
	rlsErr    error
)

// SharedRLSPostgres returns a super (admin) DSN and an app-role DSN.
//
// Both point at the SAME shared, fully-migrated database. Kept for tests that
// want both handles in one line; new tests can just call SharedPostgresDSN and,
// where privilege is genuinely needed, SharedSuperuserDSN.
func SharedRLSPostgres(t *testing.T) (superDSN, appDSN string) {
	t.Helper()
	return SharedSuperuserDSN(t), SharedPostgresDSN(t)
}

// sharedAppDSN provisions (once) and returns the openrails_app DSN on the shared
// integration database.
func sharedAppDSN(t *testing.T) string {
	t.Helper()
	superDSN := sharedPrivilegedDSN(t)
	rlsOnce.Do(func() {
		if rlsErr = enableAppRoleLogin(context.Background(), superDSN); rlsErr != nil {
			return
		}
		rlsAppDSN, rlsErr = withUserInfo(superDSN, appRole, appPassword)
	})
	require.NoError(t, rlsErr, "provision shared RLS app role")
	return rlsAppDSN
}

var (
	rlsGuardOnce sync.Once
	rlsGuardErr  error
)

// requireRLSEnforcing fails the test unless dsn authenticates as a role RLS
// actually constrains — NOT rolsuper AND NOT rolbypassrls. Without it,
// repointing the default at superuser would leave every test passing and
// meaning nothing. Checked once per process; the DSN is a process constant.
func requireRLSEnforcing(t *testing.T, dsn string) {
	t.Helper()
	rlsGuardOnce.Do(func() { rlsGuardErr = checkRLSEnforcing(context.Background(), dsn) })
	require.NoError(t, rlsGuardErr, "dbtest: the DEFAULT integration DSN must enforce RLS")
}

func checkRLSEnforcing(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("open default integration dsn: %w", err)
	}
	defer pool.Close()
	var (
		user   string
		bypass bool
	)
	if err := pool.QueryRow(ctx, `
		SELECT current_user,
		       COALESCE(bool_or(rolsuper OR rolbypassrls), TRUE)
		  FROM pg_roles WHERE rolname = current_user`).Scan(&user, &bypass); err != nil {
		return fmt.Errorf("read RLS posture: %w", err)
	}
	if bypass {
		return fmt.Errorf(
			"dbtest: SharedPostgresDSN handed back the privileged role %q (rolsuper or rolbypassrls). "+
				"The default integration DSN MUST connect as %s so per-merchant RLS policies constrain the test "+
				"exactly as in production. A test that genuinely needs privilege calls SharedSuperuserDSN(t)",
			user, appRole)
	}
	return nil
}

// appRoleLoginLockKey serializes the ALTER ROLE below across every test process
// sharing one Postgres server. Roles are CLUSTER-wide, so `go test -p N` has N
// processes altering the same pg_authid tuple at once, which Postgres rejects
// with `tuple concurrently updated` (XX000) — the ALTER is idempotent but not
// concurrency-safe. An arbitrary fixed key; only this function takes it.
const appRoleLoginLockKey = 8670001

// enableAppRoleLogin grants the openrails_app role LOGIN + a password so tests can
// connect as it. Migration 001 creates the role NOLOGIN; production attaches login
// credentials out of band. The role keeps NOBYPASSRLS so RLS still enforces.
//
// Serialized on a cluster-wide advisory lock, and retried, because the role is
// shared by every concurrently running test package.
func enableAppRoleLogin(ctx context.Context, superDSN string) error {
	sqlDB, err := sql.Open("pgx", superDSN)
	if err != nil {
		return fmt.Errorf("open super dsn: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	var lastErr error
	for attempt := range 5 {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
		lastErr = func() error {
			conn, err := sqlDB.Conn(ctx)
			if err != nil {
				return fmt.Errorf("acquire conn: %w", err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, appRoleLoginLockKey); err != nil {
				return fmt.Errorf("take app-role lock: %w", err)
			}
			defer func() {
				_, _ = conn.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, appRoleLoginLockKey)
			}()
			if _, err := conn.ExecContext(ctx, `ALTER ROLE `+appRole+` WITH LOGIN PASSWORD '`+appPassword+`'`); err != nil {
				return fmt.Errorf("grant %s login: %w", appRole, err)
			}
			return nil
		}()
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

// withUserInfo returns dsn with its userinfo replaced by user:password.
func withUserInfo(dsn, user, password string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.User = url.UserPassword(user, password)
	return u.String(), nil
}
