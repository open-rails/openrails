//go:build integration

package controlplane

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	authhttp "github.com/open-rails/authkit/http"
	jwtkit "github.com/open-rails/authkit/jwt"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/tenant"
)

// This suite exercises the FEDERATED tenant-signed delegated-token path end to
// end against a real Postgres + live JWKS servers (issue #259): multi-issuer-per-
// tenant resolution, cross-tenant rejection, the per-issuer kill-switch, and the
// tenant-admin permission tier. The pure catalog/SSRF/probe decisions are covered
// by the non-integration tests; this proves the wiring (registry -> verifier ->
// resolve) actually holds when keys come from JWKS and tenants come from the DB.

const fedSchemaDDL = `
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
CREATE TABLE IF NOT EXISTS billing.tenant_delegated_issuers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES billing.tenants (id) ON DELETE CASCADE,
    issuer      TEXT NOT NULL,
    jwks_uri    TEXT NOT NULL,
    audiences   TEXT[] NOT NULL DEFAULT '{}',
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    CONSTRAINT uq_fed_issuer UNIQUE (issuer)
);
`

func mustTenantID(s string) tenant.ID {
	id, err := tenant.ParseID(s)
	if err != nil {
		panic(err)
	}
	return id
}

var (
	fedTenantA = mustTenantID("00000000-0000-0000-0000-0000000000aa")
	fedTenantB = mustTenantID("00000000-0000-0000-0000-0000000000bb")
)

func newFedTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	// Escape hatch for environments where the testcontainers dynamic-port/ryuk
	// path is flaky (e.g. a busy Docker host): point at an existing Postgres via
	// OPENRAILS_TEST_DB_DSN and skip spinning a container.
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool, err := pgxpool.New(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
		_, err = pool.Exec(ctx, fedSchemaDDL)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `
			INSERT INTO billing.tenants (id, slug, name, authkit_tenant_slug) VALUES
			  ($1, 'tenant-a', 'Tenant A', 'org-a'),
			  ($2, 'tenant-b', 'Tenant B', 'org-b')
			ON CONFLICT (id) DO NOTHING
		`, fedTenantA.String(), fedTenantB.String())
		require.NoError(t, err)
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

	_, err = pool.Exec(ctx, fedSchemaDDL)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO billing.tenants (id, slug, name, authkit_tenant_slug) VALUES
		  ($1, 'tenant-a', 'Tenant A', 'org-a'),
		  ($2, 'tenant-b', 'Tenant B', 'org-b')
		ON CONFLICT (id) DO NOTHING
	`, fedTenantA.String(), fedTenantB.String())
	require.NoError(t, err)
	return pool
}

// jwksServer serves a single signer's public key as a JWKS document.
func jwksServer(t *testing.T, signer *jwtkit.RSASigner) *httptest.Server {
	t.Helper()
	ks := jwtkit.JWKS{Keys: []jwtkit.JWK{
		jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), "RS256"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwtkit.ServeJWKS(w, r, ks)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newFedControlPlane builds a minimal ControlPlane wired to a pool + a federated
// (JWKS) verifier, with NO self-issuer seed (this suite tests federated only).
func newFedControlPlane(t *testing.T, pool *pgxpool.Pool) *ControlPlane {
	t.Helper()
	v := authhttp.NewVerifier(
		authhttp.WithTenantMode("multi"),
		authhttp.WithPermissionCatalog(func(perms []string) error {
			if len(perms) == 0 {
				return errors.New("no perms")
			}
			for _, p := range perms {
				if !IsDelegatedPermission(p) {
					return errors.New("forbidden perm: " + p)
				}
			}
			return nil
		}),
	)
	return &ControlPlane{
		cfg:                &config.Config{Env: "test"},
		pool:               pool,
		delegatedVerifier:  v,
		issuer:             "https://openrails.self.test",
		delegatedAudiences: []string{"openrails"},
		delegatedIssuers:   map[string]struct{}{},
	}
}

func mintFed(t *testing.T, signer *jwtkit.RSASigner, issuer, sub string, perms []string, tenantClaim string) string {
	t.Helper()
	tok, err := authhttp.MintDelegatedAccessToken(context.Background(), signer, authhttp.DelegatedAccessParams{
		Issuer:           issuer,
		Audiences:        []string{"openrails"},
		DelegatedSubject: sub,
		Permissions:      perms,
		Tenant:           tenantClaim,
		TTL:              5 * time.Minute,
	})
	require.NoError(t, err)
	return tok
}

func registerIssuerRow(t *testing.T, pool *pgxpool.Pool, tid tenant.ID, issuer, jwksURI string, enabled bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO billing.tenant_delegated_issuers (tenant_id, issuer, jwks_uri, audiences, enabled)
		VALUES ($1, $2, $3, ARRAY['openrails-test'], $4)
		ON CONFLICT (issuer) DO UPDATE SET jwks_uri = EXCLUDED.jwks_uri, audiences = EXCLUDED.audiences, enabled = EXCLUDED.enabled
	`, tid.String(), issuer, jwksURI, enabled)
	require.NoError(t, err)
}

func truncateIssuers(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `TRUNCATE billing.tenant_delegated_issuers`)
	require.NoError(t, err)
}

// TestFederatedDelegatedTokens drives every federated scenario as a subtest over
// ONE shared Postgres container (each subtest truncates the issuer registry and
// builds its own verifier), so the suite spins a single container, not one per
// case.
func TestFederatedDelegatedTokens(t *testing.T) {
	pool := newFedTestPool(t)

	t.Run("MultiIssuerPerTenant_And_CrossTenant", func(t *testing.T) {
		truncateIssuers(t, pool)
		federatedMultiIssuer(t, pool)
	})
	t.Run("KillSwitch_EvictsOneIssuerNotSiblings", func(t *testing.T) {
		truncateIssuers(t, pool)
		federatedKillSwitch(t, pool)
	})
	t.Run("TenantAdminPermissionResolves", func(t *testing.T) {
		truncateIssuers(t, pool)
		federatedTenantAdmin(t, pool)
	})
	t.Run("UnregisteredIssuerRejected", func(t *testing.T) {
		truncateIssuers(t, pool)
		federatedUnregistered(t, pool)
	})
	t.Run("RegisterRoute_GlobalUniqueness", func(t *testing.T) {
		truncateIssuers(t, pool)
		federatedGlobalUniqueness(t, pool)
	})
}

func federatedMultiIssuer(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	cp := newFedControlPlane(t, pool)

	// Tenant A has TWO issuers (multiple host apps analogue): distinct keys, one
	// tenant, shared users. Tenant B has its own issuer.
	sgA1, _ := jwtkit.NewRSASigner(2048, "a1-kid")
	sgA2, _ := jwtkit.NewRSASigner(2048, "a2-kid")
	sgB, _ := jwtkit.NewRSASigner(2048, "b-kid")
	issA1, issA2, issB := "https://a1.test", "https://a2.test", "https://b.test"

	registerIssuerRow(t, pool, fedTenantA, issA1, jwksServer(t, sgA1).URL, true)
	registerIssuerRow(t, pool, fedTenantA, issA2, jwksServer(t, sgA2).URL, true)
	registerIssuerRow(t, pool, fedTenantB, issB, jwksServer(t, sgB).URL, true)
	require.NoError(t, cp.reloadDelegatedIssuers(ctx))

	// Both of tenant A's issuers, with the SAME delegated_sub, resolve to the SAME
	// tenant + same billing subject (shared-user-namespace requirement).
	for _, iss := range []struct {
		issuer string
		signer *jwtkit.RSASigner
	}{{issA1, sgA1}, {issA2, sgA2}} {
		tok := mintFed(t, iss.signer, iss.issuer, "shared-user-1", []string{PermSelfBillingRead}, "org-a")
		res, err := cp.ResolveDelegated(ctx, tok)
		require.NoError(t, err, "issuer %s", iss.issuer)
		require.Equal(t, fedTenantA, res.TenantID)
		require.Equal(t, "shared-user-1", res.DelegatedSubject)
		require.Equal(t, iss.issuer, res.Issuer)
	}

	// Tenant B's issuer resolves to B (distinct).
	tokB := mintFed(t, sgB, issB, "b-user", []string{PermSelfBillingRead}, "org-b")
	resB, err := cp.ResolveDelegated(ctx, tokB)
	require.NoError(t, err)
	require.Equal(t, fedTenantB, resB.TenantID)

	// CROSS-TENANT: tenant A's issuer naming tenant B in its `tenant` claim is
	// rejected — a tenant-signed token can never assert a tenant other than the
	// one its (globally-unique) issuer is pinned to.
	forged := mintFed(t, sgA1, issA1, "evil", []string{PermSelfBillingRead}, "org-b")
	_, err = cp.ResolveDelegated(ctx, forged)
	require.ErrorIs(t, err, ErrDelegatedInvalid)
}

func federatedKillSwitch(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	cp := newFedControlPlane(t, pool)

	sgA1, _ := jwtkit.NewRSASigner(2048, "a1-kid")
	sgA2, _ := jwtkit.NewRSASigner(2048, "a2-kid")
	issA1, issA2 := "https://a1.test", "https://a2.test"
	registerIssuerRow(t, pool, fedTenantA, issA1, jwksServer(t, sgA1).URL, true)
	registerIssuerRow(t, pool, fedTenantA, issA2, jwksServer(t, sgA2).URL, true)
	require.NoError(t, cp.reloadDelegatedIssuers(ctx))

	tokA1 := mintFed(t, sgA1, issA1, "u", []string{PermSelfBillingRead}, "org-a")
	tokA2 := mintFed(t, sgA2, issA2, "u", []string{PermSelfBillingRead}, "org-a")
	_, err := cp.ResolveDelegated(ctx, tokA1)
	require.NoError(t, err)

	// Kill issuer A1. Its sibling A2 (same tenant) must stay live.
	require.NoError(t, cp.DisableDelegatedIssuer(ctx, fedTenantA, issA1))

	_, err = cp.ResolveDelegated(ctx, tokA1)
	require.Error(t, err, "disabled issuer's tokens must be rejected")

	_, err = cp.ResolveDelegated(ctx, tokA2)
	require.NoError(t, err, "sibling issuer must remain live after kill-switch")
}

func federatedTenantAdmin(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	cp := newFedControlPlane(t, pool)

	sg, _ := jwtkit.NewRSASigner(2048, "admin-kid")
	iss := "https://admin.test"
	registerIssuerRow(t, pool, fedTenantA, iss, jwksServer(t, sg).URL, true)
	require.NoError(t, cp.reloadDelegatedIssuers(ctx))

	// The acting admin is the delegated_sub; the token carries a tenant-admin perm.
	tok := mintFed(t, sg, iss, "admin-user", []string{PermTenantBillingRead, PermTenantEntitlementsWrite}, "org-a")
	res, err := cp.ResolveDelegated(ctx, tok)
	require.NoError(t, err)
	require.Equal(t, fedTenantA, res.TenantID)
	require.Equal(t, "admin-user", res.DelegatedSubject)
	require.True(t, res.HasPermission(PermTenantEntitlementsWrite))
}

func federatedUnregistered(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	cp := newFedControlPlane(t, pool)

	// A perfectly-valid signer whose issuer was never registered. Its key is not
	// in the verifier, so it fails closed.
	sg, _ := jwtkit.NewRSASigner(2048, "ghost-kid")
	tok := mintFed(t, sg, "https://ghost.test", "u", []string{PermSelfBillingRead}, "org-a")
	_, err := cp.ResolveDelegated(ctx, tok)
	require.Error(t, err)
}

func federatedGlobalUniqueness(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	cp := newFedControlPlane(t, pool)

	sg, _ := jwtkit.NewRSASigner(2048, "k")
	js := jwksServer(t, sg).URL
	iss := "https://shared-iss.test"

	// Tenant A registers it.
	require.NoError(t, cp.RegisterDelegatedIssuer(ctx, RegisterDelegatedIssuerParams{
		TenantID: fedTenantA, Issuer: iss, JWKSURI: js, Audiences: []string{"openrails-test"},
	}))
	// Tenant B cannot claim the same globally-unique issuer.
	err := cp.RegisterDelegatedIssuer(ctx, RegisterDelegatedIssuerParams{
		TenantID: fedTenantB, Issuer: iss, JWKSURI: js, Audiences: []string{"openrails-test"},
	})
	require.ErrorIs(t, err, ErrIssuerOwnedByOtherTenant)

	// Tenant A rotating its own issuer (same issuer, new JWKS) succeeds.
	sg2, _ := jwtkit.NewRSASigner(2048, "k2")
	require.NoError(t, cp.RegisterDelegatedIssuer(ctx, RegisterDelegatedIssuerParams{
		TenantID: fedTenantA, Issuer: iss, JWKSURI: jwksServer(t, sg2).URL, Audiences: []string{"openrails-test"},
	}))
}
