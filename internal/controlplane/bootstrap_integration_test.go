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
// plane updates. OpenRails 001_schema.up.sql owns the full table in production; the
// control-plane bootstrap only needs the default row to exist.
const minimalTenantsDDL = `
CREATE SCHEMA IF NOT EXISTS billing;
CREATE TABLE IF NOT EXISTS billing.tenants (
    id               UUID PRIMARY KEY,
    slug             TEXT NOT NULL,
    name             TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'active',
    authkit_tenant_id   TEXT,
    authkit_tenant_slug TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at       TIMESTAMPTZ
);
INSERT INTO billing.tenants (id, slug, name)
VALUES ('00000000-0000-0000-0000-000000000001', 'default', 'Default')
ON CONFLICT (id) DO NOTHING;

CREATE TABLE IF NOT EXISTS billing.tenant_delegated_issuers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES billing.tenants (id) ON DELETE CASCADE,
    issuer      TEXT NOT NULL,
    jwks_uri    TEXT NOT NULL,
    audiences   TEXT[] NOT NULL DEFAULT '{}',
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    CONSTRAINT uq_bootstrap_delegated_issuer UNIQUE (issuer)
);
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
			ControlPlane: &config.ControlPlaneConfig{
				Enabled:     true,
				Issuer:      "https://billing.test",
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

	// First run: creates the operator tenant, seeds role/perms, mints service token, records tenant.
	res1, err := cp.Bootstrap(ctx, BootstrapOptions{MintInitialServiceToken: true})
	require.NoError(t, err)
	require.NotNil(t, res1)
	require.True(t, res1.TenantCreated, "first run should create the bootstrap (default) tenant org")
	require.True(t, res1.ServiceTokenMinted, "first run should mint the initial admin service token")
	require.NotEmpty(t, res1.ServiceTokenSecret)
	require.NotEmpty(t, res1.BootstrapTenantID)

	// HARDCUT (#312): the default tenant's own AuthKit org is recorded — there is
	// no separate "operator" tenant.
	var authKitTenantSlug, authKitTenantID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT authkit_tenant_slug, authkit_tenant_id FROM billing.tenants WHERE id = $1::uuid`,
		tenant.DefaultID.String()).Scan(&authKitTenantSlug, &authKitTenantID))
	require.Equal(t, tenant.DefaultSlug, authKitTenantSlug)
	require.Equal(t, res1.BootstrapTenantID, authKitTenantID)

	// The admin role holds the per-tenant catalog (full catalog EXCEPT the
	// cross-tenant platform-superadmin permission, #226).
	perms, err := cp.Core().GetRolePermissions(ctx, tenant.DefaultSlug, OperatorRole)
	require.NoError(t, err)
	require.ElementsMatch(t, OperatorRolePermissions(), perms)
	require.NotContains(t, perms, PermPlatformSuperadmin)

	// Second run: idempotent. No new org, no new service token.
	res2, err := cp.Bootstrap(ctx, BootstrapOptions{MintInitialServiceToken: true})
	require.NoError(t, err)
	require.NotNil(t, res2)
	require.False(t, res2.TenantCreated, "re-run must not recreate the bootstrap tenant org")
	require.False(t, res2.ServiceTokenMinted, "re-run must not mint a second service token")
	require.Empty(t, res2.ServiceTokenSecret)
	require.Equal(t, res1.BootstrapTenantID, res2.BootstrapTenantID)

	// Exactly one service token exists after two runs.
	serviceTokens, err := cp.Core().ListServiceTokens(ctx, tenant.DefaultSlug)
	require.NoError(t, err)
	require.Len(t, serviceTokens, 1, "exactly one admin service token after two bootstrap runs")
	require.ElementsMatch(t, []string{ResourceKindTenant}, resourceKinds(serviceTokens[0].Resources))
	require.Contains(t, resourceIDs(serviceTokens[0].Resources, ResourceKindTenant), tenant.DefaultID.String())

	resolved, err := cp.ResolveServiceToken(ctx, res1.ServiceTokenSecret)
	require.NoError(t, err)
	require.Equal(t, tenant.DefaultID, resolved.TenantID)
	require.Contains(t, resourceIDs(resolved.Resources, ResourceKindTenant), tenant.DefaultID.String())
}

func resourceKinds(resources []authcore.ServiceTokenResource) []string {
	out := make([]string, 0, len(resources))
	for _, r := range resources {
		out = append(out, r.Kind)
	}
	return out
}

func resourceIDs(resources []authcore.ServiceTokenResource, kind string) []string {
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

	_, err := cp.Bootstrap(ctx, BootstrapOptions{MintInitialServiceToken: false})
	require.NoError(t, err)

	// Every per-tenant operator permission is granted to the operator role and
	// shows up as an effective role permission; the cross-tenant platform
	// superadmin permission is NOT (it is seeded only to the platform role, #226).
	eff, err := cp.Core().EffectiveRolePermissions(ctx, tenant.DefaultSlug, OperatorRole)
	require.NoError(t, err)
	for _, want := range OperatorRolePermissions() {
		require.Containsf(t, eff, want, "operator role should effectively hold %q", want)
	}
	require.NotContains(t, eff, PermPlatformSuperadmin)

	// Re-running with the same catalog keeps the grant stable (replace, not grow).
	_, err = cp.Bootstrap(ctx, BootstrapOptions{MintInitialServiceToken: false})
	require.NoError(t, err)
	eff2, err := cp.Core().EffectiveRolePermissions(ctx, tenant.DefaultSlug, OperatorRole)
	require.NoError(t, err)
	require.ElementsMatch(t, eff, eff2, fmt.Sprintf("permissions should be stable across reruns: %v vs %v", eff, eff2))
}

// TestBootstrapPlatform_SeedsSuperadminInSeparateTenant proves the #226 platform
// layer: BootstrapPlatform seeds a SEPARATE platform tenant whose role holds ONLY
// openrails:platform:superadmin, and HasPlatformSuperadmin authorizes a member
// of that org while a tenant operator admin (in the operator tenant) is denied.
func TestBootstrapPlatform_SeedsSuperadminInSeparateTenant(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)

	cfg := &config.Config{
		Env: "test",
		Auth: &config.AuthConfig{
			ExpectedAudience: "openrails-app",
			ControlPlane: &config.ControlPlaneConfig{
				Enabled:            true,
				Issuer:             "https://billing.test",
				TokenPrefix:        "openrails",
				PlatformTenantSlug: "openrails-platform",
			},
		},
	}
	cp, err := New(ctx, cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)

	// Bootstrap the tenant operator tenant AND the platform tenant.
	_, err = cp.Bootstrap(ctx, BootstrapOptions{MintInitialServiceToken: false})
	require.NoError(t, err)
	pres, err := cp.BootstrapPlatform(ctx)
	require.NoError(t, err)
	require.NotNil(t, pres)
	require.Equal(t, "openrails-platform", pres.PlatformTenantSlug)

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

	// A per-tenant admin (openrails:admin in their OWN tenant) is NOT a platform
	// superadmin: HasPlatformSuperadmin evaluates the platform tenant, where they are
	// not a member.
	require.NoError(t, cp.Core().AddMember(ctx, tenant.DefaultSlug, tenantOperatorAdminID))
	require.NoError(t, cp.Core().AssignRole(ctx, tenant.DefaultSlug, tenantOperatorAdminID, OperatorRole))
	opIsAdmin, err := cp.IsAdmin(ctx, tenant.DefaultSlug, tenantOperatorAdminID)
	require.NoError(t, err)
	require.True(t, opIsAdmin, "per-tenant admin should hold openrails:admin in their own tenant")
	opIsPlatform, err := cp.HasPlatformSuperadmin(ctx, tenantOperatorAdminID)
	require.NoError(t, err)
	require.False(t, opIsPlatform, "per-tenant admin must NOT be a platform superadmin")

	// Idempotent re-run.
	_, err = cp.BootstrapPlatform(ctx)
	require.NoError(t, err)
}

// TestDelegatedTenantResolution exercises the AuthKit-tenant -> OpenRails-tenant
// mapping ResolveServiceToken relies on (tenantForAuthKitTenant): resolution
// keys EXCLUSIVELY on the IMMUTABLE AuthKit tenant uuid (authkit_tenant_id) —
// there is no slug fallback — and rejects credentials without a uuid, unlinked
// directory rows, and unknown or suspended AuthKit tenants.
func TestDelegatedTenantResolution(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newEnabledControlPlane(t, pool)

	// A second, suspended tenant owned by a distinct AuthKit tenant (uuid+slug linked).
	_, err := pool.Exec(ctx, `
		INSERT INTO billing.tenants (id, slug, name, status, authkit_tenant_id, authkit_tenant_slug)
		VALUES ('00000000-0000-0000-0000-000000000002', 'acme', 'Acme', 'suspended', 'ak-acme-id', 'acme-org')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	// A third, active tenant written BEFORE the uuid dual-write: slug only, NULL uuid.
	_, err = pool.Exec(ctx, `
		INSERT INTO billing.tenants (id, slug, name, status, authkit_tenant_slug)
		VALUES ('00000000-0000-0000-0000-000000000003', 'beta', 'Beta', 'active', 'beta-org')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	// Map the default tenant to an active AuthKit tenant (uuid + slug) so the
	// happy path resolves.
	_, err = pool.Exec(ctx, `
		UPDATE billing.tenants
		   SET authkit_tenant_id = 'ak-default-id', authkit_tenant_slug = 'default-org', status = 'active'
		 WHERE id = $1::uuid`, tenant.DefaultID.String())
	require.NoError(t, err)

	// Immutable uuid -> its OpenRails tenant. The slug is irrelevant to
	// resolution (presentation/audit only).
	tid, slug, err := cp.tenantForAuthKitTenant(ctx, "ak-default-id")
	require.NoError(t, err)
	require.Equal(t, tenant.DefaultID, tid)
	require.Equal(t, "default", slug)

	// Hard cut: a credential without a tenant uuid is rejected — there is no
	// slug fallback, even when a slug-matching directory row exists.
	_, _, err = cp.tenantForAuthKitTenant(ctx, "")
	require.ErrorIs(t, err, ErrServiceTokenTenantUnresolved)

	// Hard cut: a uuid that matches no LINKED directory row is rejected — an
	// unlinked row (authkit_tenant_id IS NULL) is unreachable regardless of its
	// slug.
	_, _, err = cp.tenantForAuthKitTenant(ctx, "ak-beta-id")
	require.ErrorIs(t, err, ErrServiceTokenTenantUnresolved)

	// An unmapped uuid never reaches a row linked to a DIFFERENT AuthKit
	// tenant (slug-reassignment confusion is structurally impossible).
	_, _, err = cp.tenantForAuthKitTenant(ctx, "ak-other-id")
	require.ErrorIs(t, err, ErrServiceTokenTenantUnresolved)

	// Suspended tenant -> cross-tenant/unmapped rejection.
	_, _, err = cp.tenantForAuthKitTenant(ctx, "ak-acme-id")
	require.ErrorIs(t, err, ErrServiceTokenTenantUnresolved)

	// The control plane built a delegated verifier (browser-tier prerequisite).
	require.NotNil(t, cp.DelegatedVerifier())
}
