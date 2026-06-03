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

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"github.com/uptrace/bun"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/tenant"
)

// This suite proves the migration-050 Row Level Security DESIGN actually enforces
// cross-tenant isolation when the app connects as the unprivileged openrails_app
// role (issue #227). It replicates 050's exact policy form on a probe table and
// drives it through the real db.DB + RunInTenantTx, so it validates both the
// policy semantics AND the GUC plumbing — not a reimplementation of either.

var (
	rlsTenantA = mustID("00000000-0000-0000-0000-0000000000a1")
	rlsTenantB = mustID("00000000-0000-0000-0000-0000000000b2")
)

func mustID(s string) tenant.ID {
	id, err := tenant.ParseID(s)
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

// rlsProbe is the tenant-owned probe table, mirroring a real billing.* table.
type rlsProbe struct {
	bun.BaseModel `bun:"table:billing.rls_probe"`
	ID            string `bun:"id,pk"`
	TenantID      string `bun:"tenant_id"`
	Val           string `bun:"val"`
}

const rlsSetupDDL = `
CREATE SCHEMA IF NOT EXISTS billing;
CREATE TABLE IF NOT EXISTS billing.rls_probe (
    id        UUID PRIMARY KEY,
    tenant_id UUID NOT NULL,
    val       TEXT NOT NULL
);
-- Exact migration-050 policy form.
ALTER TABLE billing.rls_probe ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.rls_probe FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON billing.rls_probe;
CREATE POLICY tenant_isolation ON billing.rls_probe
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);
-- Unprivileged application role (migration-050 form) WITH LOGIN for the test.
	DO $$ BEGIN
	    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
	        CREATE ROLE openrails_app NOBYPASSRLS;
	    END IF;
	END $$;
	ALTER ROLE openrails_app WITH LOGIN PASSWORD 'app_pw';
	GRANT USAGE ON SCHEMA billing TO openrails_app;
	GRANT SELECT, INSERT, UPDATE, DELETE ON billing.rls_probe TO openrails_app;
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

func TestRLSEnforcement_Under_OpenRailsAppRole(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := startRLSContainer(t)

	// As the superuser: create schema/table/policy/role and seed both tenants
	// (the superuser bypasses RLS, so it can write any tenant's rows).
	super := newDBRetry(t, superDSN)
	defer super.Close()
	_, err := super.GetDB().ExecContext(ctx, rlsSetupDDL)
	require.NoError(t, err)
	_, err = super.GetDB().ExecContext(ctx, `DELETE FROM billing.rls_probe WHERE id IN (?::uuid, ?::uuid)`,
		"00000000-0000-0000-0000-00000000000a",
		"00000000-0000-0000-0000-00000000000b",
	)
	require.NoError(t, err)
	seed := []rlsProbe{
		{ID: "00000000-0000-0000-0000-00000000000a", TenantID: rlsTenantA.String(), Val: "a-row"},
		{ID: "00000000-0000-0000-0000-00000000000b", TenantID: rlsTenantB.String(), Val: "b-row"},
	}
	_, err = super.GetDB().NewInsert().Model(&seed).Exec(ctx)
	require.NoError(t, err)

	// The superuser BYPASSES RLS -> posture is non-enforcing, and a required
	// posture must fail.
	superPosture, err := super.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.False(t, superPosture.Enforcing, "superuser must report non-enforcing")
	require.Error(t, super.EnforceRLSPosture(ctx, true))

	// As openrails_app: RLS ENFORCES.
	app := newDBRetry(t, appDSN)
	defer app.Close()
	appPosture, err := app.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.True(t, appPosture.Enforcing, "openrails_app must report enforcing")
	require.NoError(t, app.EnforceRLSPosture(ctx, true))

	// (1) Without the GUC set, the app sees NOTHING (fail-closed) — a query that
	// forgets the tenant cannot leak another tenant's rows.
	var leaked []rlsProbe
	require.NoError(t, app.GetDB().NewSelect().Model(&leaked).Scan(ctx))
	require.Len(t, leaked, 0, "no GUC => zero rows visible (fail-closed)")

	// (2) Inside a tenant-A tx, only tenant A's row is visible.
	ctxA := tenant.WithID(ctx, rlsTenantA)
	err = app.RunInTenantTx(ctxA, func(ctx context.Context, tx bun.Tx) error {
		var rows []rlsProbe
		if err := tx.NewSelect().Model(&rows).Scan(ctx); err != nil {
			return err
		}
		require.Len(t, rows, 1)
		require.Equal(t, "a-row", rows[0].Val)
		return nil
	})
	require.NoError(t, err)

	// (3) Tenant A cannot read OR delete tenant B's row (it is simply invisible).
	ctxB := tenant.WithID(ctx, rlsTenantB)
	require.NoError(t, app.RunInTenantTx(ctxA, func(ctx context.Context, tx bun.Tx) error {
		res, derr := tx.NewDelete().Model((*rlsProbe)(nil)).Where("val = ?", "b-row").Exec(ctx)
		require.NoError(t, derr)
		n, _ := res.RowsAffected()
		require.Equal(t, int64(0), n, "tenant A must not be able to delete tenant B's row")
		return nil
	}))
	// Confirm tenant B's row still exists (visible only under tenant B's GUC).
	require.NoError(t, app.RunInTenantTx(ctxB, func(ctx context.Context, tx bun.Tx) error {
		n, derr := tx.NewSelect().Model((*rlsProbe)(nil)).Count(ctx)
		require.NoError(t, derr)
		require.Equal(t, 1, n)
		return nil
	}))

	// (4) WITH CHECK: inside a tenant-A tx, inserting a row stamped with tenant B's
	// id is rejected — a tenant cannot write into another tenant's namespace.
	err = app.RunInTenantTx(ctxA, func(ctx context.Context, tx bun.Tx) error {
		_, ierr := tx.NewInsert().Model(&rlsProbe{
			ID: "00000000-0000-0000-0000-00000000000c", TenantID: rlsTenantB.String(), Val: "cross",
		}).Exec(ctx)
		return ierr
	})
	require.Error(t, err, "WITH CHECK must reject a cross-tenant insert")
}
