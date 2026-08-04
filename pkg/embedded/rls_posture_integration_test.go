//go:build integration

package embedded

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #763: embedded construction never enforced the RLS posture gate
// (db.EnforceRLSPosture, issue #227) — only the standalone boot path did.
// internal/app.buildRuntimeWithOverrides now runs the check for BOTH
// construction paths from one call site, so embedded.New (the entry point
// every real embedding host uses) inherits it automatically. These tests
// drive that through the actual public entry point, not the internal
// function directly, so a future refactor that re-splits the two paths would
// be caught here.

// TestNew_EmbeddedBootRefusesBypassRLSRoleOutsideDev proves the non-dev fail
// case: an embedded host whose injected PGXPool connects as a
// superuser/BYPASSRLS role must NOT boot outside development.
func TestNew_EmbeddedBootRefusesBypassRLSRoleOutsideDev(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), superDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:               "staging",
		TestMode:          config.CredentialPostureLive,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		DB:                &config.DBConfig{URL: superDSN},
	}
	_, err = New(Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.Error(t, err, "an embedded host connected as a BYPASSRLS role must refuse to boot outside development")
	require.ErrorContains(t, err, "bypasses RLS")
	require.ErrorContains(t, err, "openrails_app")
}

// TestNew_EmbeddedBootWarnsOnBypassRLSRoleInDev proves development stays a
// warn, not a boot failure, exactly like the standalone path.
func TestNew_EmbeddedBootWarnsOnBypassRLSRoleInDev(t *testing.T) {
	superDSN, _ := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), superDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:      "development",
		TestMode: config.CredentialPostureSandbox,
		DB:       &config.DBConfig{URL: superDSN},
	}
	e, err := New(Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.NoError(t, err, "development must only warn, never fail, on a bypass-RLS role")
	t.Cleanup(func() { _ = e.Close(context.Background()) })
}

// TestNew_EmbeddedBootSucceedsAsAppRoleOutsideDev proves the positive case: a
// properly RLS-enforcing role boots cleanly outside development.
func TestNew_EmbeddedBootSucceedsAsAppRoleOutsideDev(t *testing.T) {
	_, appDSN := dbtest.SharedRLSPostgres(t)
	pool, err := pgxpool.New(context.Background(), appDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	cfg := &config.Config{
		Env:               "staging",
		TestMode:          config.CredentialPostureLive,
		ProviderWriteMode: config.ProviderWriteModeReadOnly,
		DB:                &config.DBConfig{URL: appDSN},
	}
	e, err := New(Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
	require.NoError(t, err, "an RLS-enforcing role must boot outside development")
	t.Cleanup(func() { _ = e.Close(context.Background()) })
}
