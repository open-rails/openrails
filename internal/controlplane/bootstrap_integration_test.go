//go:build integration

package controlplane

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	authpgmigrations "github.com/open-rails/authkit/migrations/postgres"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/tenant"
)

// minimalTenantsDDL creates just the billing.tenants directory row the control
// plane updates. OpenRails migration 039 owns the full table in production; the
// control-plane bootstrap only needs the default row to exist.
const minimalTenantsDDL = `
CREATE SCHEMA IF NOT EXISTS billing;
CREATE TABLE IF NOT EXISTS billing.tenants (
    id               UUID PRIMARY KEY,
    slug             TEXT NOT NULL,
    name             TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'active',
    authkit_org_id   TEXT,
    authkit_org_slug TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);
INSERT INTO billing.tenants (id, slug, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default')
ON CONFLICT (id) DO NOTHING;
`

func newBootstrapTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)

	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	// Apply AuthKit profiles.* schema in filename order, then billing.tenants.
	entries, err := authpgmigrations.FS.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		b, rerr := authpgmigrations.FS.ReadFile(name)
		require.NoError(t, rerr)
		_, eerr := pool.Exec(ctx, string(b))
		require.NoErrorf(t, eerr, "apply authkit migration %s", name)
	}
	_, err = pool.Exec(ctx, minimalTenantsDDL)
	require.NoError(t, err)
	return pool
}

func newEnabledControlPlane(t *testing.T, pool *pgxpool.Pool) *ControlPlane {
	t.Helper()
	cfg := &config.Config{
		Env: "test",
		Auth: &config.AuthConfig{
			ExpectedAudience: "openrails-app",
			OperatorOrgSlug:  "operator",
			ControlPlane: &config.ControlPlaneConfig{
				Enabled:     true,
				Issuer:      "https://billing.test",
				OrgMode:     "multi",
				TokenPrefix: "openrails",
			},
		},
	}
	cp, err := New(context.Background(), cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)
	return cp
}

func TestBootstrap_Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newEnabledControlPlane(t, pool)

	// First run: creates org, seeds role/perms, mints OAT, records tenant.
	res1, err := cp.Bootstrap(ctx, BootstrapOptions{MintInitialOAT: true})
	require.NoError(t, err)
	require.NotNil(t, res1)
	require.True(t, res1.OrgCreated, "first run should create the operator org")
	require.True(t, res1.OATMinted, "first run should mint the initial operator OAT")
	require.NotEmpty(t, res1.OATSecret)
	require.NotEmpty(t, res1.OperatorOrgID)

	// Tenant directory records the operator org.
	var orgSlug, orgID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT authkit_org_slug, authkit_org_id FROM billing.tenants WHERE id = $1::uuid`,
		tenant.DefaultID.String()).Scan(&orgSlug, &orgID))
	require.Equal(t, "operator", orgSlug)
	require.Equal(t, res1.OperatorOrgID, orgID)

	// Operator role holds the full catalog.
	perms, err := cp.Core().GetRolePermissions(ctx, "operator", OperatorRole)
	require.NoError(t, err)
	require.ElementsMatch(t, CatalogNames(), perms)

	// Second run: idempotent. No new org, no new OAT.
	res2, err := cp.Bootstrap(ctx, BootstrapOptions{MintInitialOAT: true})
	require.NoError(t, err)
	require.NotNil(t, res2)
	require.False(t, res2.OrgCreated, "re-run must not recreate the operator org")
	require.False(t, res2.OATMinted, "re-run must not mint a second OAT")
	require.Empty(t, res2.OATSecret)
	require.Equal(t, res1.OperatorOrgID, res2.OperatorOrgID)

	// Exactly one OAT exists after two runs.
	oats, err := cp.Core().ListOrgAccessTokens(ctx, "operator")
	require.NoError(t, err)
	require.Len(t, oats, 1, "exactly one operator OAT after two bootstrap runs")
}

func TestBootstrap_SeedsPermissionCatalog(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newEnabledControlPlane(t, pool)

	_, err := cp.Bootstrap(ctx, BootstrapOptions{MintInitialOAT: false})
	require.NoError(t, err)

	// Every catalog permission is granted to the operator role and shows up as
	// an effective role permission.
	eff, err := cp.Core().EffectiveRolePermissions(ctx, "operator", OperatorRole)
	require.NoError(t, err)
	for _, want := range CatalogNames() {
		require.Containsf(t, eff, want, "operator role should effectively hold %q", want)
	}

	// Re-running with the same catalog keeps the grant stable (replace, not grow).
	_, err = cp.Bootstrap(ctx, BootstrapOptions{MintInitialOAT: false})
	require.NoError(t, err)
	eff2, err := cp.Core().EffectiveRolePermissions(ctx, "operator", OperatorRole)
	require.NoError(t, err)
	require.ElementsMatch(t, eff, eff2, fmt.Sprintf("permissions should be stable across reruns: %v vs %v", eff, eff2))
}
