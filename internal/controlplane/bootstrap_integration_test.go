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
)

// minimalMerchantsDDL creates just the openrails.merchants directory table the
// control plane updates (#480/#481). OpenRails 001_schema.up.sql owns the full
// table in production; the control-plane bootstrap only needs the table (plus a
// row, seeded via dbtest.EnsureTestMerchant) to exist. unbound host names are unique;
// EnsureTestMerchant settles by its immutable test UUID. permission_group_id is the AuthKit
// merchant permission-group id that administers the merchant.
const minimalMerchantsDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;
CREATE TABLE IF NOT EXISTS openrails.merchants (
    id               UUID PRIMARY KEY,
    slug             TEXT NOT NULL,
    display_name     TEXT,
    status           TEXT NOT NULL DEFAULT 'active',
    permission_group_id  TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at       TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_merchants_unbound_slug ON openrails.merchants(slug) WHERE deleted_at IS NULL AND permission_group_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_merchants_permission_group_id ON openrails.merchants(permission_group_id) WHERE permission_group_id IS NOT NULL;
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

// enrollTestMFA marks userID as MFA-enrolled directly in the DB (a settings
// row + a default email factor — exactly what authkit's userHasEnabledMFA
// checks), so granting MFA-required root roles (authkit v0.84.0 #49) succeeds
// in fixtures that have no session to enroll 2FA through the real HTTP flow.
func enrollTestMFA(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO profiles.mfa_settings (user_id, enabled) VALUES ($1::uuid, true)
		 ON CONFLICT (user_id) DO UPDATE SET enabled = true`, userID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO profiles.mfa_factors (user_id, method, is_default) VALUES ($1::uuid, 'email', true)`, userID)
	require.NoError(t, err)
}

func newTestControlPlane(t *testing.T, pool *pgxpool.Pool) *ControlPlane {
	t.Helper()
	cfg := &config.Config{
		// "dev": authkit v0.78.0 resolves signing keys fail-closed outside dev
		// (no keys.json -> no ephemeral keys -> IssueAccessToken missing_signer).
		Env:  "dev",
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

	// First run: creates the merchant permission-group, seeds role/perms, mints
	// an API key, and records the merchant group id.
	res1, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.NotNil(t, res1)
	require.True(t, res1.MerchantGroupCreated, "first run should create the merchant group")
	require.True(t, res1.APIKeyMinted, "first run should mint the initial admin API key")
	require.NotEmpty(t, res1.APIKeySecret)
	require.NotEmpty(t, res1.BootstrapMerchantGroupID)

	// #567: the merchant permission-group's internal id is recorded on the
	// merchant directory row via the permission_group_id column.
	var groupID string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT permission_group_id FROM openrails.merchants WHERE id = $1::uuid`,
		dbtest.TestMerchantID.String()).Scan(&groupID))
	require.Equal(t, res1.BootstrapMerchantGroupID, groupID)

	// #567: an admin assigned the merchant `owner` role auto-holds `merchant:*`,
	// so it can perform any merchant operation (here: admit) but never a
	// foreign-persona perm.
	// (No InitialAdminUserID was seeded above, so a fresh owner check uses Can on a
	// known subject below in TestBootstrap_SeedsPermissionCatalog.)

	// Second run: idempotent. No new group, no new API key.
	res2, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.NotNil(t, res2)
	require.False(t, res2.MerchantGroupCreated, "re-run must not recreate the merchant group")
	require.False(t, res2.APIKeyMinted, "re-run must not mint a second API key")
	require.Empty(t, res2.APIKeySecret)
	require.Equal(t, res1.BootstrapMerchantGroupID, res2.BootstrapMerchantGroupID)

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

// TestBootstrap_RevokedKeysNeverAutoRemint is #747's core regression guard: an
// operator who revokes every API key for a merchant group (e.g. after a
// suspected compromise) must never have a fresh one silently minted by a later
// Bootstrap call — not even one that explicitly requests minting. Eligibility
// is the merchant group's FULL key history, not just its current live count.
func TestBootstrap_RevokedKeysNeverAutoRemint(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	// Genuine first run: no key history yet, explicit request mints.
	res1, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.True(t, res1.APIKeyMinted, "genuine first run should mint")
	require.NotEmpty(t, res1.APIKeySecret)

	// Operator revokes the key.
	keys, err := cp.Core().ListAPIKeys(ctx, MerchantType, dbtest.TestMerchantSlug)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	revoked, err := cp.Core().RevokeAPIKey(ctx, MerchantType, dbtest.TestMerchantSlug, keys[0].ID)
	require.NoError(t, err)
	require.True(t, revoked)

	// Sanity: zero LIVE keys now, but the key HISTORY is non-empty.
	afterRevoke, err := cp.Core().ListAPIKeys(ctx, MerchantType, dbtest.TestMerchantSlug)
	require.NoError(t, err)
	require.Len(t, afterRevoke, 1)
	require.NotNil(t, afterRevoke[0].RevokedAt)

	// A routine re-Bootstrap WITHOUT a mint request must not mint (trivially).
	res2, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: false})
	require.NoError(t, err)
	require.False(t, res2.APIKeyMinted)

	// #747's actual fix: an EXPLICIT mint request must ALSO not auto-heal the
	// revocation — this merchant group has had a key before, so it is not a
	// first-run state, regardless of what the caller asks for.
	res3, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.False(t, res3.APIKeyMinted, "an explicit mint request must never auto-heal a merchant that previously had a key")
	require.Empty(t, res3.APIKeySecret)

	// Exactly one API key exists ever (the original, revoked) — no replacement minted.
	finalKeys, err := cp.Core().ListAPIKeys(ctx, MerchantType, dbtest.TestMerchantSlug)
	require.NoError(t, err)
	require.Len(t, finalKeys, 1, "no replacement key should have been minted after revocation")
}

// TestBootstrap_FreshMerchantMintsOnlyOnExplicitRequest proves the OTHER half
// of #747's fix still works for a merchant that never had a key: the zero
// value (MintInitialAPIKey: false) never mints, and an explicit true does.
func TestBootstrap_FreshMerchantMintsOnlyOnExplicitRequest(t *testing.T) {
	ctx := context.Background()
	pool := newBootstrapTestPool(t)
	cp := newTestControlPlane(t, pool)

	res1, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: false})
	require.NoError(t, err)
	require.False(t, res1.APIKeyMinted, "the zero value must never mint")
	keys, err := cp.Core().ListAPIKeys(ctx, MerchantType, dbtest.TestMerchantSlug)
	require.NoError(t, err)
	require.Empty(t, keys)

	res2, err := cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, MintInitialAPIKey: true})
	require.NoError(t, err)
	require.True(t, res2.APIKeyMinted, "a genuinely first-run merchant mints on explicit request")
	require.NotEmpty(t, res2.APIKeySecret)
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
	_, err = cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, InitialAdminUserID: adminUser, MintInitialAPIKey: false})
	require.NoError(t, err)

	// #567: the merchant `owner` (auto-holds `merchant:*`) effectively holds every
	// merchant catalog permission, evaluated via the group walk-up. It never holds
	// a platform permission.
	for _, want := range []string{
		PermMerchantSettingsRead, PermMerchantSettingsUpdate,
		PermMerchantPaymentProvidersRead, PermMerchantPaymentProvidersUpdate,
		PermMerchantCatalogRead, PermMerchantCatalogUpdate,
		PermMerchantCustomerSettingsRead, PermMerchantCustomerSettingsUpdate,
		PermMerchantPaymentsRead, PermMerchantPaymentsRefund,
		PermMerchantSubscriptionsRead, PermMerchantSubscriptionsUpdate,
		PermMerchantAdmissionsCreate, PermMerchantUsageRead, PermMerchantRepairAlertsRead,
	} {
		ok, cerr := cp.Core().Can(ctx, adminUser, authcore.SubjectKindUser, MerchantType, dbtest.TestMerchantSlug, want)
		require.NoError(t, cerr)
		require.Truef(t, ok, "merchant owner should effectively hold %q", want)
	}
	platformDenied, err := cp.Core().Can(ctx, adminUser, authcore.SubjectKindUser, MerchantType, dbtest.TestMerchantSlug, "platform:merchants:delete")
	require.NoError(t, err)
	require.False(t, platformDenied, "merchant owner must not reach platform permissions")

	// Re-running keeps the grant stable (owner still holds merchant:*).
	_, err = cp.Bootstrap(ctx, BootstrapOptions{BootstrapMerchantSlug: dbtest.TestMerchantSlug, InitialAdminUserID: adminUser, MintInitialAPIKey: false})
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

	token, _, err := cp.Core().MintAccessToken(ctx, owner.ID, nil)
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
		BootstrapMerchantSlug: dbtest.TestMerchantSlug,
		InitialAdminUserID:    merchantOwner.ID,
		MintInitialAPIKey:     false,
	})
	require.NoError(t, err)

	rootOperator, err := cp.Core().CreateUser(ctx, "root-operator@example.test", "rootoperator")
	require.NoError(t, err)
	// authkit v0.84.0 #49: assigning an MFA-required root role fails closed
	// unless the subject already has 2FA enrolled.
	enrollTestMFA(ctx, t, pool, rootOperator.ID)
	require.NoError(t, cp.Core().Genesis().AssignGroupRole(ctx, authcore.RootPersona, "", rootOperator.ID, authcore.SubjectKindUser, authcore.OwnerRoleName))

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
