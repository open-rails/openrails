//go:build integration

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authcore "github.com/open-rails/authkit/embedded"
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
// EnsureTestMerchant's ON CONFLICT (slug). permission_group_id is the #481 ownership
// link (the AuthKit org that administers the merchant), NOT an identity-equation.
const minimalMerchantsDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;
CREATE TABLE IF NOT EXISTS openrails.merchants (
    id               UUID PRIMARY KEY,
    slug             TEXT NOT NULL UNIQUE,
    status           TEXT NOT NULL DEFAULT 'active',
    permission_group_id  TEXT,
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

	// #567: the merchant permission-group's internal id is recorded on the
	// merchant directory row via the permission_group_id column (repurposed to hold the
	// group id, no longer an org uuid).
	var groupID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT permission_group_id FROM openrails.merchants WHERE id = $1::uuid`,
		dbtest.TestMerchantID.String()).Scan(&groupID))
	require.Equal(t, res1.BootstrapOrgID, groupID)

	// #567: an admin assigned the merchant `owner` role auto-holds `merchant:*`,
	// so it can perform any merchant operation (here: admit) but never a
	// foreign-persona perm.
	// (No InitialAdminUserID was seeded above, so a fresh owner check uses Can on a
	// known subject below in TestBootstrap_SeedsPermissionCatalog.)

	// Second run: idempotent. No new group, no new API key.
	res2, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.NotNil(t, res2)
	require.False(t, res2.OrgCreated, "re-run must not recreate the merchant group")
	require.False(t, res2.APIKeyMinted, "re-run must not mint a second API key")
	require.Empty(t, res2.APIKeySecret)
	require.Equal(t, res1.BootstrapOrgID, res2.BootstrapOrgID)

	// Exactly one API key exists after two runs (under the merchant group).
	apiKeys, err := cp.Core().ListAPIKeys(ctx, MerchantType, dbtest.TestMerchantSlug)
	require.NoError(t, err)
	require.Len(t, apiKeys, 1, "exactly one admin API key after two bootstrap runs")

	// #569 (hard cut): API-key resource scopes are gone. The bootstrapped key's
	// merchant identity is the permission group it was minted under, so resolving it
	// yields the merchant directly — there is no merchant resource scope to assert.
	resolved, err := cp.ResolveAPIKey(ctx, res1.APIKeySecret)
	require.NoError(t, err)
	require.Equal(t, dbtest.TestMerchantID, resolved.MerchantID)
}

func TestBootstrap_SeedsPermissionCatalog(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	// #567: a subject_kind='user' role assignment requires a real profiles.users
	// row (authkit v0.50.0 trg_group_assignment_subject_fk trigger). Create the
	// admin as a real user before bootstrapping it as the merchant owner — the
	// production path bootstraps an already-registered admin.
	adminRec, err := cp.Core().CreateUser(ctx, "bootstrap-admin@example.test", "bootstrapadmin")
	require.NoError(t, err)
	adminUser := adminRec.ID
	_, err = cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, InitialAdminUserID: adminUser, MintInitialAPIKey: false})
	require.NoError(t, err)

	// #567: the merchant `owner` (auto-holds `merchant:*`) effectively holds every
	// merchant catalog permission, evaluated via the group walk-up. It never holds
	// a platform permission.
	for _, want := range MerchantOwnerRolePermissions() {
		ok, cerr := cp.Core().Can(ctx, adminUser, authcore.SubjectKindUser, MerchantType, dbtest.TestMerchantSlug, want)
		require.NoError(t, cerr)
		require.Truef(t, ok, "merchant owner should effectively hold %q", want)
	}
	platformDenied, err := cp.Core().Can(ctx, adminUser, authcore.SubjectKindUser, MerchantType, dbtest.TestMerchantSlug, "platform:merchants:delete")
	require.NoError(t, err)
	require.False(t, platformDenied, "merchant owner must not reach platform permissions")

	// Re-running keeps the grant stable (owner still holds merchant:*).
	_, err = cp.Bootstrap(ctx, BootstrapOptions{BootstrapOrgSlug: dbtest.TestMerchantSlug, InitialAdminUserID: adminUser, MintInitialAPIKey: false})
	require.NoError(t, err)
	stillOwner, err := cp.Core().Can(ctx, adminUser, authcore.SubjectKindUser, MerchantType, dbtest.TestMerchantSlug, PermMerchantAdmissionsCreate)
	require.NoError(t, err)
	require.True(t, stillOwner, "merchant owner grant stable across reruns")
}

func TestEnsureCustomerPermissionGroup_Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	owner, err := cp.Core().CreateUser(ctx, "customer-owner@example.test", "customerowner")
	require.NoError(t, err)
	customerID := owner.ID

	groupID, err := cp.EnsureCustomerPermissionGroup(ctx, customerID, owner.ID)
	require.NoError(t, err)
	require.NotEmpty(t, groupID)

	canSpend, err := cp.Core().Can(ctx, owner.ID, authcore.SubjectKindUser, CustomerType, customerID, PermCustomerSpendDelegationsUpdate)
	require.NoError(t, err)
	require.True(t, canSpend, "customer owner should hold customer:*")
	canMerchant, err := cp.Core().Can(ctx, owner.ID, authcore.SubjectKindUser, CustomerType, customerID, PermMerchantAdmissionsCreate)
	require.NoError(t, err)
	require.False(t, canMerchant, "customer owner must not hold merchant perms")

	again, err := cp.EnsureCustomerPermissionGroup(ctx, customerID, owner.ID)
	require.NoError(t, err)
	require.Equal(t, groupID, again)
}

func TestGeneratedCustomerRemoteApplicationRoute_LazyCreatesGroup(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	owner, err := cp.Core().CreateUser(ctx, "customer-remote-app-owner@example.test", "customerremoteappowner")
	require.NoError(t, err)
	customerID := owner.ID

	_, err = cp.Core().ResolveGroupIDForSlug(ctx, CustomerType, customerID)
	require.ErrorIs(t, err, authkit.ErrGroupNotFound)

	token, _, err := cp.Core().IssueAccessToken(ctx, owner.ID, "customer-remote-app-owner@example.test", nil)
	require.NoError(t, err)

	mux := http.NewServeMux()
	for _, spec := range cp.RouteSpecs() {
		mux.Handle(spec.Method+" "+spec.Path, spec.Handler)
	}

	body := map[string]any{
		"slug":     "cozy-art-ci",
		"issuer":   "https://cozy.art",
		"jwks_uri": "https://cozy.art/.well-known/jwks.json",
		"enabled":  true,
	}
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/customer/"+customerID+"/remote-applications", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	groupID, err := cp.Core().ResolveGroupIDForSlug(ctx, CustomerType, customerID)
	require.NoError(t, err)
	require.NotEmpty(t, groupID)
	app, err := cp.Core().GetRemoteApplication(ctx, "https://cozy.art")
	require.NoError(t, err)
	require.Equal(t, groupID, app.PermissionGroupID)
	require.Equal(t, "https://cozy.art", app.Issuer)
	require.Equal(t, "cozy-art-ci", app.Slug)
}

func TestRootOperatorBoundary_ReachNotMerchantCapability(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	merchantOwner, err := cp.Core().CreateUser(ctx, "merchant-owner@example.test", "merchantowner")
	require.NoError(t, err)
	_, err = cp.Bootstrap(ctx, BootstrapOptions{
		BootstrapOrgSlug:   dbtest.TestMerchantSlug,
		InitialAdminUserID: merchantOwner.ID,
		MintInitialAPIKey:  false,
	})
	require.NoError(t, err)

	rootOperator, err := cp.Core().CreateUser(ctx, "root-operator@example.test", "rootoperator")
	require.NoError(t, err)
	require.NoError(t, cp.Core().AssignGroupRole(ctx, authcore.RootPersona, "", rootOperator.ID, authcore.SubjectKindUser, authcore.OwnerRoleName))

	canModerateMerchant, err := cp.Core().Can(ctx, rootOperator.ID, authcore.SubjectKindUser, authcore.RootPersona, "", "root:merchants:delete")
	require.NoError(t, err)
	require.True(t, canModerateMerchant, "root owner should hold root moderation authority")
	canRunMerchant, err := cp.Core().Can(ctx, rootOperator.ID, authcore.SubjectKindUser, MerchantType, dbtest.TestMerchantSlug, PermMerchantSettingsUpdate)
	require.NoError(t, err)
	require.False(t, canRunMerchant, "root authority must not imply merchant internals")

	merchantCanRunMerchant, err := cp.Core().Can(ctx, merchantOwner.ID, authcore.SubjectKindUser, MerchantType, dbtest.TestMerchantSlug, PermMerchantSettingsUpdate)
	require.NoError(t, err)
	require.True(t, merchantCanRunMerchant, "merchant owner should hold merchant:*")
	merchantCanModerateRoot, err := cp.Core().Can(ctx, merchantOwner.ID, authcore.SubjectKindUser, authcore.RootPersona, "", "root:merchants:delete")
	require.NoError(t, err)
	require.False(t, merchantCanModerateRoot, "merchant owner must not hold root moderation perms")
}

// TestMerchantForOwnerOrg exercised the #481/#500 org→merchant ownership link
// (merchantForGroupID / AuthorizeMerchant). Under #567 a merchant IS its own
// permission-group and the org↔merchant coupling is gone — API keys resolve their
// merchant from the permission group the key was minted under (#569 removed the
// resource-scope mechanism entirely), not from an owning org. The legacy helpers
// remain for source-compat but their org-identity premise is obsolete, so this
// test is retired.
func TestMerchantForOwnerOrg(t *testing.T) {
	t.Skip("org→merchant ownership link removed under #567; merchant is resolved from the permission group the API key was minted under")
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	// The default test merchant is owned by AuthKit org "ak-default-id".
	_, err := pool.Exec(ctx, `
		UPDATE openrails.merchants
		   SET permission_group_id = 'ak-default-id', status = 'active'
		 WHERE id = $1::uuid`, dbtest.TestMerchantID.String())
	require.NoError(t, err)
	// A SECOND merchant owned by the SAME org (#481: one org -> many merchants).
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status, permission_group_id)
		VALUES ('00000000-0000-0000-0000-000000000004', 'second', 'active', 'ak-default-id')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	// A deleted merchant owned by a distinct org.
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status, permission_group_id, deleted_at)
		VALUES ('00000000-0000-0000-0000-000000000002', 'acme', 'deleted', 'ak-acme-id', current_timestamp)
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)
	// A single-merchant owner still supports the inferred common path.
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.merchants (id, slug, status, permission_group_id)
		VALUES ('00000000-0000-0000-0000-000000000003', 'solo', 'active', 'ak-solo-id')
		ON CONFLICT (id) DO NOTHING`)
	require.NoError(t, err)

	// Single owning org -> its merchant namespace (role-based ownership; not identity).
	mid, _, err := cp.merchantForGroupID(ctx, "ak-solo-id")
	require.NoError(t, err)
	require.False(t, mid.IsZero(), "single owning org resolves to its merchant")

	// An org that owns multiple merchants must name one explicitly.
	_, _, err = cp.merchantForGroupID(ctx, "ak-default-id")
	require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)

	// A credential with no owning org is rejected.
	_, _, err = cp.merchantForGroupID(ctx, "")
	require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)

	// An owner that owns no merchant is rejected.
	_, _, err = cp.merchantForGroupID(ctx, "ak-nobody-id")
	require.ErrorIs(t, err, ErrServiceCredentialMerchantUnresolved)

	// A deleted merchant's owner resolves to no merchant.
	_, _, err = cp.merchantForGroupID(ctx, "ak-acme-id")
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
