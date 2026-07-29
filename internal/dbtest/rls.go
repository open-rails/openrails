//go:build integration

package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"testing"

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
// Both point at the SAME shared, fully-migrated database — so a package that uses
// both helpers still provisions only one container. The super DSN bypasses RLS
// (use it to seed cross-merchant fixtures); the app DSN is the same DSN
// SharedPostgresDSN hands out by default.
//
// Prefer SharedPostgresDSN + SharedSuperuserDSN(t, reason) in new tests: this
// helper predates the default inversion and its super DSN carries no stated
// reason.
func SharedRLSPostgres(t *testing.T) (superDSN, appDSN string) {
	t.Helper()
	return SharedSuperuserDSN(t, "SharedRLSPostgres: seeds cross-merchant fixtures alongside the RLS-enforcing app DSN"),
		SharedPostgresDSN(t)
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

// requireRLSEnforcing fails the test unless dsn authenticates as a role that RLS
// actually constrains — i.e. NOT rolsuper AND NOT rolbypassrls.
//
// This is the harness-level guard that makes "the test bypassed RLS" impossible
// to reintroduce silently. Five shipped guards read as protection and could not
// fire because their tests connected as superuser (SEC-16, or#824, or#860,
// or#861, or#862); the default handle is now checked before it is handed out.
// Checked once per test-binary process — the DSN is a process-level constant.
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
				"exactly as they do in production. If this test genuinely needs privilege, call "+
				"SharedSuperuserDSN(t, \"<reason>\") explicitly instead of widening the default",
			user, appRole)
	}
	return nil
}

// enableAppRoleLogin grants the openrails_app role LOGIN + a password so tests can
// connect as it. Migration 001 creates the role NOLOGIN; production attaches login
// credentials out of band. The role keeps NOBYPASSRLS so RLS still enforces.
func enableAppRoleLogin(ctx context.Context, superDSN string) error {
	sqlDB, err := sql.Open("pgx", superDSN)
	if err != nil {
		return fmt.Errorf("open super dsn: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if _, err := sqlDB.ExecContext(ctx, `ALTER ROLE `+appRole+` WITH LOGIN PASSWORD '`+appPassword+`'`); err != nil {
		return fmt.Errorf("grant %s login: %w", appRole, err)
	}
	return nil
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
