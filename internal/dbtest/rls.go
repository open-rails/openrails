//go:build integration

package dbtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/stretchr/testify/require"
)

const (
	// appRole is the unprivileged, NOBYPASSRLS role created by 001_schema.up.sql that
	// production connects as so the per-tenant RLS policies actually constrain
	// queries. RLS tests connect as this role to prove enforcement.
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

// SharedRLSPostgres returns a super (admin) DSN and an app-role DSN for RLS tests.
//
// Both point at the SAME shared, fully-migrated database as SharedPostgresDSN — so
// a package that uses both helpers still provisions only one container. The super
// DSN bypasses RLS (use it to seed cross-tenant fixtures); the app DSN connects as
// the openrails_app role (NOBYPASSRLS, created by 001_schema.up.sql) that the RLS
// policies enforce against.
func SharedRLSPostgres(t *testing.T) (superDSN, appDSN string) {
	t.Helper()
	superDSN = SharedPostgresDSN(t)
	rlsOnce.Do(func() {
		if rlsErr = enableAppRoleLogin(context.Background(), superDSN); rlsErr != nil {
			return
		}
		rlsAppDSN, rlsErr = withUserInfo(superDSN, appRole, appPassword)
	})
	require.NoError(t, rlsErr, "provision shared RLS app role")
	return superDSN, rlsAppDSN
}

// enableAppRoleLogin grants the openrails_app role LOGIN + a password so tests can
// connect as it. Migration 050 creates the role NOLOGIN; production attaches login
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
