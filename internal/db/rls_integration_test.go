//go:build integration

package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This suite proves the migration-050 Row Level Security DESIGN actually enforces
// cross-merchant isolation when the app connects as the unprivileged openrails_app
// role (issue #227). It replicates 050's exact policy form on a probe table and
// drives it through the real db.DB GUC plumbing (TestRLSEnforcement_PgxSide,
// in rls_pgx_integration_test.go) — not a reimplementation of either.

var (
	rlsTenantA = mustID("00000000-0000-0000-0000-0000000000a1")
	rlsTenantB = mustID("00000000-0000-0000-0000-0000000000b2")
)

func mustID(s string) merchant.ID {
	id, err := merchant.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}

// newDBRetry opens a *db.DB, retrying transient connect failures. Busy/CI Docker
// hosts intermittently drop bridge packets (i/o timeout) on connect; the test
// logic is unaffected, so a few retries keep the suite from flaking.
func newDBRetry(t *testing.T, dsn string) *DB {
	t.Helper()
	var lastErr error
	for i := 0; i < 8; i++ {
		d, err := NewDB(&config.DBConfig{URL: dsn})
		if err == nil {
			return d
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	require.NoError(t, lastErr)
	return nil
}

const rlsSetupDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;
CREATE TABLE IF NOT EXISTS openrails.rls_probe (
    id        UUID PRIMARY KEY,
    merchant_id UUID NOT NULL,
    val       TEXT NOT NULL
);
-- Exact migration-050 policy form.
ALTER TABLE openrails.rls_probe ENABLE ROW LEVEL SECURITY;
ALTER TABLE openrails.rls_probe FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.rls_probe;
CREATE POLICY merchant_isolation ON openrails.rls_probe
    USING      (merchant_id = nullif(current_setting('app.merchant_id', true), '')::uuid)
    WITH CHECK (merchant_id = nullif(current_setting('app.merchant_id', true), '')::uuid);
-- Unprivileged application role (migration-050 form) WITH LOGIN for the test.
	DO $$ BEGIN
	    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
	        CREATE ROLE openrails_app NOBYPASSRLS;
	    END IF;
	END $$;
	ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw';
	GRANT USAGE ON SCHEMA openrails TO openrails_app;
	GRANT SELECT, INSERT, UPDATE, DELETE ON openrails.rls_probe TO openrails_app;
	`

func startRLSContainer(t *testing.T) (superDSN string, appDSN string) {
	t.Helper()
	ctx := context.Background()

	// Escape hatch for flaky-testcontainers hosts: OPENRAILS_TEST_DB_DSN is the
	// SUPER/admin DSN; the openrails_app DSN is derived by swapping the userinfo
	// (the setup DDL creates that role with password 'app_pw').
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		u, err := url.Parse(dsn)
		require.NoError(t, err)
		superDSN = dsn
		u.User = url.UserPassword("openrails_app", "app_pw")
		appDSN = u.String()
		return superDSN, appDSN
	}

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("super"),
		postgres.WithPassword("super"),
		testcontainers.WithHostConfigModifier(postgresTestLimits),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)

	superDSN = fmt.Sprintf("postgresql://super:super@%s:%s/openrails?sslmode=disable", host, port.Port())
	appDSN = fmt.Sprintf("postgresql://openrails_app:app_pw@%s:%s/openrails?sslmode=disable", host, port.Port())
	return superDSN, appDSN
}

func postgresTestLimits(hc *container.HostConfig) {
	hc.Resources.Memory = 2 << 30
	hc.Resources.NanoCPUs = 2_000_000_000
}

// TestRLSPosture_Reporting proves CheckRLSPosture/EnforceRLSPosture classify
// privileged vs unprivileged roles correctly. The enforcement semantics
// themselves (fail-closed visibility, WITH CHECK, GUC scoping) are covered by
// TestRLSEnforcement_PgxSide.
func TestRLSPosture_Reporting(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := startRLSContainer(t)

	super := newDBRetry(t, superDSN)
	defer super.Close()
	_, err := super.Pool().Exec(ctx, rlsSetupDDL)
	require.NoError(t, err)

	// The superuser BYPASSES RLS -> posture is non-enforcing, and the gate must
	// fail (unconditionally — there is no environment argument, or#782).
	superPosture, err := super.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.False(t, superPosture.Enforcing, "superuser must report non-enforcing")
	require.Error(t, super.EnforceRLSPosture(ctx))

	// As openrails_app: RLS ENFORCES.
	app := newDBRetry(t, appDSN)
	defer app.Close()
	appPosture, err := app.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.True(t, appPosture.Enforcing, "openrails_app must report enforcing")
	require.NoError(t, app.EnforceRLSPosture(ctx))
}
