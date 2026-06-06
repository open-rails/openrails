//go:build integration

package controlplane

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/core"
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
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at       TIMESTAMPTZ
);
INSERT INTO billing.tenants (id, slug, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default')
ON CONFLICT (id) DO NOTHING;
`

func newBootstrapTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalPostgresTestPool(t, dsn, "openrails_bootstrap")
		applyBootstrapTestSchema(t, ctx, pool)
		return pool
	}

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

	applyBootstrapTestSchema(t, ctx, pool)
	return pool
}

func applyBootstrapTestSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
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

	// Operator role holds the per-tenant operator catalog (full catalog EXCEPT
	// the cross-tenant platform-superadmin permission, #226).
	perms, err := cp.Core().GetRolePermissions(ctx, "operator", OperatorRole)
	require.NoError(t, err)
	require.ElementsMatch(t, OperatorRolePermissions(), perms)
	require.NotContains(t, perms, PermPlatformSuperadmin)

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
	require.ElementsMatch(t, []string{ResourceKindTenant}, resourceKinds(oats[0].Resources))
	require.Contains(t, resourceIDs(oats[0].Resources, ResourceKindTenant), tenant.DefaultID.String())

	resolved, err := cp.ResolveOAT(ctx, res1.OATSecret)
	require.NoError(t, err)
	require.Equal(t, tenant.DefaultID, resolved.TenantID)
	require.Contains(t, resourceIDs(resolved.Resources, ResourceKindTenant), tenant.DefaultID.String())
}

func resourceKinds(resources []authcore.OrgAccessTokenResource) []string {
	out := make([]string, 0, len(resources))
	for _, r := range resources {
		out = append(out, r.Kind)
	}
	return out
}

func resourceIDs(resources []authcore.OrgAccessTokenResource, kind string) []string {
	out := make([]string, 0, len(resources))
	for _, r := range resources {
		if r.Kind == kind {
			out = append(out, r.ID)
		}
	}
	return out
}

func TestBootstrap_SeedsPermissionCatalog(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newEnabledControlPlane(t, pool)

	_, err := cp.Bootstrap(ctx, BootstrapOptions{MintInitialOAT: false})
	require.NoError(t, err)

	// Every per-tenant operator permission is granted to the operator role and
	// shows up as an effective role permission; the cross-tenant platform
	// superadmin permission is NOT (it is seeded only to the platform role, #226).
	eff, err := cp.Core().EffectiveRolePermissions(ctx, "operator", OperatorRole)
	require.NoError(t, err)
	for _, want := range OperatorRolePermissions() {
		require.Containsf(t, eff, want, "operator role should effectively hold %q", want)
	}
	require.NotContains(t, eff, PermPlatformSuperadmin)

	// Re-running with the same catalog keeps the grant stable (replace, not grow).
	_, err = cp.Bootstrap(ctx, BootstrapOptions{MintInitialOAT: false})
	require.NoError(t, err)
	eff2, err := cp.Core().EffectiveRolePermissions(ctx, "operator", OperatorRole)
	require.NoError(t, err)
	require.ElementsMatch(t, eff, eff2, fmt.Sprintf("permissions should be stable across reruns: %v vs %v", eff, eff2))
}

// TestBootstrapPlatform_SeedsSuperadminInSeparateOrg proves the #226 platform
// layer: BootstrapPlatform seeds a SEPARATE platform org whose role holds ONLY
// openrails:platform:superadmin, and HasPlatformSuperadmin authorizes a member
// of that org while a tenant operator admin (in the operator org) is denied.
func TestBootstrapPlatform_SeedsSuperadminInSeparateOrg(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)

	cfg := &config.Config{
		Env: "test",
		Auth: &config.AuthConfig{
			ExpectedAudience: "openrails-app",
			OperatorOrgSlug:  "operator",
			ControlPlane: &config.ControlPlaneConfig{
				Enabled:         true,
				Issuer:          "https://billing.test",
				OrgMode:         "multi",
				TokenPrefix:     "openrails",
				PlatformOrgSlug: "openrails-platform",
			},
		},
	}
	cp, err := New(ctx, cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)

	// Bootstrap the tenant operator org AND the platform org.
	_, err = cp.Bootstrap(ctx, BootstrapOptions{MintInitialOAT: false})
	require.NoError(t, err)
	pres, err := cp.BootstrapPlatform(ctx)
	require.NoError(t, err)
	require.NotNil(t, pres)
	require.Equal(t, "openrails-platform", pres.PlatformOrgSlug)

	// The platform role holds ONLY the platform-superadmin permission.
	perms, err := cp.Core().GetRolePermissions(ctx, "openrails-platform", PlatformRole)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{PermPlatformSuperadmin}, perms)

	// AuthKit user IDs are UUIDs of real profiles.users rows; create them.
	pAdmin, err := cp.Core().CreateUser(ctx, "platform-admin@test.local", "platformadmin")
	require.NoError(t, err)
	platformAdminID := pAdmin.ID
	tAdmin, err := cp.Core().CreateUser(ctx, "tenant-admin@test.local", "tenantadmin")
	require.NoError(t, err)
	tenantOperatorAdminID := tAdmin.ID

	// A platform-org member with the platform role passes HasPlatformSuperadmin.
	require.NoError(t, cp.Core().AddMember(ctx, "openrails-platform", platformAdminID))
	require.NoError(t, cp.Core().AssignRole(ctx, "openrails-platform", platformAdminID, PlatformRole))
	ok, err := cp.HasPlatformSuperadmin(ctx, platformAdminID)
	require.NoError(t, err)
	require.True(t, ok, "platform identity must hold platform superadmin")

	// A tenant OPERATOR admin (operator org, openrails:admin) is NOT a platform
	// superadmin: HasPlatformSuperadmin evaluates the platform org, where they are
	// not a member.
	require.NoError(t, cp.Core().AddMember(ctx, "operator", tenantOperatorAdminID))
	require.NoError(t, cp.Core().AssignRole(ctx, "operator", tenantOperatorAdminID, OperatorRole))
	opIsAdmin, err := cp.IsOperatorAdmin(ctx, "operator", tenantOperatorAdminID)
	require.NoError(t, err)
	require.True(t, opIsAdmin, "tenant operator admin should hold operator admin")
	opIsPlatform, err := cp.HasPlatformSuperadmin(ctx, tenantOperatorAdminID)
	require.NoError(t, err)
	require.False(t, opIsPlatform, "tenant operator admin must NOT be a platform superadmin")

	// Idempotent re-run.
	_, err = cp.BootstrapPlatform(ctx)
	require.NoError(t, err)
}

// TestDelegatedTenantResolution exercises the org->tenant mapping that
// ResolveDelegated relies on for browser-direct delegated access tokens (issue
// #222 browser tier): an active org maps to its tenant, while an unknown or
// suspended org is rejected as cross-tenant/unmapped. ResolveDelegated and
// ResolveOAT share this exact mapping (tenantForOrgSlug).
func TestDelegatedTenantResolution(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newEnabledControlPlane(t, pool)

	// A second, suspended tenant owned by a distinct org.
	_, err := pool.Exec(ctx, `
		INSERT INTO billing.tenants (id, slug, name, status, authkit_org_slug)
		VALUES ('00000000-0000-0000-0000-000000000002', 'acme', 'Acme', 'suspended', 'acme-org')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	// Map the default tenant to an active org so the happy path resolves.
	_, err = pool.Exec(ctx, `
		UPDATE billing.tenants SET authkit_org_slug = 'default-org', status = 'active'
		WHERE id = $1::uuid`, tenant.DefaultID.String())
	require.NoError(t, err)

	// Active org -> its tenant.
	tid, slug, err := cp.tenantForOrgSlug(ctx, "default-org")
	require.NoError(t, err)
	require.Equal(t, tenant.DefaultID, tid)
	require.Equal(t, "default", slug)

	// Suspended tenant's org -> cross-tenant/unmapped rejection.
	_, _, err = cp.tenantForOrgSlug(ctx, "acme-org")
	require.ErrorIs(t, err, ErrOATTenantUnresolved)

	// Unknown org -> rejection.
	_, _, err = cp.tenantForOrgSlug(ctx, "nobody")
	require.ErrorIs(t, err, ErrOATTenantUnresolved)

	// The control plane built a delegated verifier (browser-tier prerequisite).
	require.NotNil(t, cp.DelegatedVerifier())
}
