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
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// minimalMerchantsDDL creates just the openrails.merchants directory table the
// control plane updates (#480/#481). OpenRails 001_schema.up.sql owns the full
// table in production; the control-plane bootstrap only needs the table (plus a
// row, seeded via dbtest.EnsureTestMerchant) to exist. slug is UNIQUE to match
// EnsureTestMerchant's ON CONFLICT (slug). owner_org_id is the #481 ownership
// link (the AuthKit org that administers the merchant), NOT an identity-equation.
const minimalMerchantsDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;
CREATE TABLE IF NOT EXISTS openrails.merchants (
    id               UUID PRIMARY KEY,
    slug             TEXT NOT NULL UNIQUE,
    status           TEXT NOT NULL DEFAULT 'active',
    owner_org_id  TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at       TIMESTAMPTZ
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
		dbtest.WithPostgresLimits(),
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
	// Apply AuthKit profiles.* schema in filename order, then openrails.merchants.
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
	_, err = pool.Exec(ctx, minimalMerchantsDDL)
	require.NoError(t, err)
	dbtest.EnsureTestMerchant(ctx, t, pool)
}

func newTestControlPlane(t *testing.T, pool *pgxpool.Pool) *ControlPlane {
	t.Helper()
	cfg := &config.Config{
		Env:  "test",
		Auth: &config.AuthConfig{Issuer: "https://openrails.test"},
	}
	cp, err := New(context.Background(), cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)
	return cp
}

func TestBootstrap_Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	// First run: creates the operator org, seeds role/perms, mints an API key,
	// and records the merchant owner.
	res1, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.NotNil(t, res1)
	require.True(t, res1.OrgCreated, "first run should create the bootstrap (default) org")
	require.True(t, res1.APIKeyMinted, "first run should mint the initial admin API key")
	require.NotEmpty(t, res1.APIKeySecret)
	require.NotEmpty(t, res1.BootstrapOrgID)

	// #481: the owning AuthKit org is recorded on the merchant via the
	// owner_org_id OWNERSHIP link (not an identity-equation, not a slug column).
	var ownerOrgID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT owner_org_id FROM openrails.merchants WHERE id = $1::uuid`,
		dbtest.TestMerchantID.String()).Scan(&ownerOrgID))
	require.Equal(t, res1.BootstrapOrgID, ownerOrgID)

	// OpenRails seeds NO role of its own (#543): admin authority is the org
	// `owner` role (AuthKit built-in), which holds `org:*` — covering the full
	// OpenRails `org:` catalog but NOT the cross-merchant platform grant (#226).
	perms, err := cp.Core().GetRolePermissions(ctx, dbtest.TestMerchantSlug, OwnerRole)
	require.NoError(t, err)
	require.Contains(t, perms, PermAdmin) // org:*
	require.NotContains(t, perms, PermPlatformSuperadmin)

	// Second run: idempotent. No new org, no new API key.
	res2, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.NotNil(t, res2)
	require.False(t, res2.OrgCreated, "re-run must not recreate the bootstrap org org")
	require.False(t, res2.APIKeyMinted, "re-run must not mint a second API key")
	require.Empty(t, res2.APIKeySecret)
	require.Equal(t, res1.BootstrapOrgID, res2.BootstrapOrgID)

	// Exactly one API key exists after two runs.
	apiKeys, err := cp.Core().ListAPIKeys(ctx, dbtest.TestMerchantSlug)
	require.NoError(t, err)
	require.Len(t, apiKeys, 1, "exactly one admin API key after two bootstrap runs")
	require.ElementsMatch(t, []string{ResourceKindMerchant}, resourceKinds(apiKeys[0].Resources))
	require.Contains(t, resourceIDs(apiKeys[0].Resources, ResourceKindMerchant), dbtest.TestMerchantID.String())

	resolved, err := cp.ResolveAPIKey(ctx, res1.APIKeySecret)
	require.NoError(t, err)
	require.Equal(t, dbtest.TestMerchantID, resolved.MerchantID)
	require.Contains(t, resourceIDs(resolved.Resources, ResourceKindMerchant), dbtest.TestMerchantID.String())
}

func resourceKinds(resources []authcore.APIKeyResource) []string {
	out := make([]string, 0, len(resources))
	for _, r := range resources {
		out = append(out, r.Kind)
	}
	return out
}

func resourceIDs(resources []authcore.APIKeyResource, kind string) []string {
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
	cp := newTestControlPlane(t, pool)

	_, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: false})
	require.NoError(t, err)

	// The org `owner` role's `org:*` grant effectively expands over every per-org
	// catalog permission; the cross-merchant platform superadmin permission is NOT
	// covered (it lives in the separate `platform:` layer, #226). OpenRails seeds
	// no role of its own (#543).
	eff, err := cp.Core().EffectiveRolePermissions(ctx, dbtest.TestMerchantSlug, OwnerRole)
	require.NoError(t, err)
	for _, want := range OperatorRolePermissions() {
		require.Containsf(t, eff, want, "org owner should effectively hold %q", want)
	}
	require.NotContains(t, eff, PermPlatformSuperadmin)

	// Re-running keeps the grant stable.
	_, err = cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: false})
	require.NoError(t, err)
	eff2, err := cp.Core().EffectiveRolePermissions(ctx, dbtest.TestMerchantSlug, OwnerRole)
	require.NoError(t, err)
	require.ElementsMatch(t, eff, eff2, fmt.Sprintf("permissions should be stable across reruns: %v vs %v", eff, eff2))
}

func TestBootstrapPlatform_NoopsInSelfHostedOpenRails(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)

	cfg := &config.Config{
		Env:  "test",
		Auth: &config.AuthConfig{Issuer: "https://openrails.test"},
	}
	cp, err := New(ctx, cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)

	_, err = cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: false})
	require.NoError(t, err)
	pres, err := cp.BootstrapPlatform(ctx)
	require.NoError(t, err)
	require.Nil(t, pres)
	require.Equal(t, "", cp.PlatformOrgSlug())

	// A per-merchant admin is still not a platform superadmin. The cross-merchant
	// SaaS admin layer belongs in openrails-saas, not this self-hosted control plane.
	tAdmin, err := cp.Core().CreateUser(ctx, "merchant-admin@test.local", "tenantadmin")
	require.NoError(t, err)
	orgOperatorAdminID := tAdmin.ID
	require.NoError(t, cp.Core().AddMember(ctx, dbtest.TestMerchantSlug, orgOperatorAdminID))
	require.NoError(t, cp.Core().AssignRole(ctx, dbtest.TestMerchantSlug, orgOperatorAdminID, OwnerRole))
	opIsAdmin, err := cp.IsAdmin(ctx, dbtest.TestMerchantSlug, orgOperatorAdminID)
	require.NoError(t, err)
	require.True(t, opIsAdmin, "per-merchant admin (org owner) should hold merchant-local org authority in their own org")
	opIsPlatform, err := cp.HasPlatformSuperadmin(ctx, orgOperatorAdminID)
	require.NoError(t, err)
	require.False(t, opIsPlatform, "per-merchant admin must NOT be a platform superadmin")
}

// TestMerchantForOwnerOrg exercises the #500 ROLE-BASED merchant resolution
// merchantForOwnerOrg relies on: the merchant is resolved via the
// owner_org_id OWNERSHIP link (the AuthKit org that administers it), NOT an
// identity-equation. It covers the happy path, an unowned credential, an unknown
// owner, a suspended merchant, and authorizing a SPECIFIC merchant when one
// org owns TWO merchants.
func TestMerchantForOwnerOrg(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	// The default test merchant is owned by AuthKit org "ak-default-id".
	_, err := pool.Exec(ctx, `
		UPDATE openrails.merchants
		   SET owner_org_id = 'ak-default-id', status = 'active'
		 WHERE id = $1::uuid`, dbtest.TestMerchantID.String())
	require.NoError(t, err)
	// A SECOND merchant owned by the SAME org (#481: one org -> many merchants).
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status, owner_org_id)
		VALUES ('00000000-0000-0000-0000-000000000004', 'second', 'active', 'ak-default-id')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	// A suspended merchant owned by a distinct org.
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status, owner_org_id)
		VALUES ('00000000-0000-0000-0000-000000000002', 'acme', 'suspended', 'ak-acme-id')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	// A single-merchant owner still supports the inferred common path.
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status, owner_org_id)
		VALUES ('00000000-0000-0000-0000-000000000003', 'solo', 'active', 'ak-solo-id')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)

	// Single owning org -> its merchant namespace (role-based ownership; not identity).
	mid, _, err := cp.merchantForOwnerOrg(ctx, "ak-solo-id")
	require.NoError(t, err)
	require.False(t, mid.IsZero(), "single owning org resolves to its merchant")

	// An org that owns multiple merchants must name one explicitly.
	_, _, err = cp.merchantForOwnerOrg(ctx, "ak-default-id")
	require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)

	// A credential with no owning org is rejected.
	_, _, err = cp.merchantForOwnerOrg(ctx, "")
	require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)

	// An owner that owns no merchant is rejected.
	_, _, err = cp.merchantForOwnerOrg(ctx, "ak-nobody-id")
	require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)

	// A suspended merchant's owner resolves to no ACTIVE merchant.
	_, _, err = cp.merchantForOwnerOrg(ctx, "ak-acme-id")
	require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)

	// #481 role-based AuthorizeMerchant: the owning org may act on EITHER of its
	// merchants when the request NAMES one (an org owning many merchants).
	require.NoError(t, cp.AuthorizeMerchant(ctx, "ak-default-id", dbtest.TestMerchantID))
	second, perr := merchant.ParseID("00000000-0000-0000-0000-000000000004")
	require.NoError(t, perr)
	require.NoError(t, cp.AuthorizeMerchant(ctx, "ak-default-id", second))
	// A different org may NOT act on a merchant it does not own.
	require.ErrorIs(t, cp.AuthorizeMerchant(ctx, "ak-acme-id", dbtest.TestMerchantID), ErrServiceCredentialScopeDenied)

	// The control plane built a delegated verifier (browser-tier prerequisite).
	require.NotNil(t, cp.DelegatedVerifier())
}
