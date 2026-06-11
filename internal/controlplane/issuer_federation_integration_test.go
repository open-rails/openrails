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

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/core"
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
CREATE TABLE IF NOT EXISTS billing.tenant_subjects (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    UUID NOT NULL REFERENCES billing.tenants (id) ON DELETE CASCADE,
    issuer       TEXT NOT NULL,
    subject      TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    CONSTRAINT uq_fed_tenant_subject UNIQUE (tenant_id, issuer, subject)
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
	return mintFedWithAudience(t, signer, issuer, sub, perms, tenantClaim, []string{"openrails"})
}

func mintFedWithAudience(t *testing.T, signer *jwtkit.RSASigner, issuer, sub string, perms []string, tenantClaim string, audiences []string) string {
	t.Helper()
	// authkit v0.19.0 requires the `tenant_id` (uuid) claim at mint. OpenRails'
	// federated resolution is issuer-pinned, so the value only needs to be a
	// stable uuid for the tenant the issuer is registered to.
	tenantID := map[string]string{
		"tenant-a": fedTenantA.String(),
		"tenant-b": fedTenantB.String(),
	}[tenantClaim]
	if tenantID == "" {
		tenantID = fedTenantA.String()
	}
	tok, err := authhttp.MintDelegatedAccessToken(context.Background(), signer, authhttp.DelegatedAccessParams{
		Issuer:           issuer,
		Audiences:        audiences,
		DelegatedSubject: sub,
		Permissions:      perms,
		Tenant:           tenantClaim,
		TenantID:         tenantID,
		TTL:              5 * time.Minute,
	})
	require.NoError(t, err)
	return tok
}

func registerIssuerRow(t *testing.T, pool *pgxpool.Pool, tid tenant.ID, issuer, jwksURI string, enabled bool) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO billing.tenant_delegated_issuers (tenant_id, issuer, jwks_uri, audiences, enabled)
		VALUES ($1, $2, $3, ARRAY['openrails'], $4)
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
	t.Run("WrongAudienceRejected", func(t *testing.T) {
		truncateIssuers(t, pool)
		federatedWrongAudience(t, pool)
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

func TestTouchTenantSubjectIsIdempotentPerOIDCTuple(t *testing.T) {
	ctx := context.Background()
	pool := newFedTestPool(t)
	cp := newFedControlPlane(t, pool)

	first, err := cp.TouchTenantSubject(ctx, fedTenantA, "https://doujins.test", "user-1")
	require.NoError(t, err)
	second, err := cp.TouchTenantSubject(ctx, fedTenantA, " https://doujins.test ", " user-1 ")
	require.NoError(t, err)
	require.Equal(t, first, second, "same tenant/issuer/subject tuple must resolve to the same payable subject")

	otherIssuer, err := cp.TouchTenantSubject(ctx, fedTenantA, "https://hentai0.test", "user-1")
	require.NoError(t, err)
	require.NotEqual(t, first, otherIssuer, "issuer is part of the payable subject key")

	var count int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM billing.tenant_subjects
		 WHERE tenant_id = $1
		   AND subject = 'user-1'
	`, fedTenantA.String()).Scan(&count))
	require.Equal(t, 2, count)
}

func TestFederatedServiceJWTs(t *testing.T) {
	ctx := context.Background()
	pool := newFedTestPool(t)
	cp := newFedControlPlane(t, pool)

	signer, err := jwtkit.NewRSASigner(2048, "service-kid")
	require.NoError(t, err)
	issuer := "https://service.example"
	subject := "service:doujins-runtime"
	registerIssuerRow(t, pool, fedTenantA, issuer, jwksServer(t, signer).URL, true)
	require.NoError(t, cp.reloadDelegatedIssuers(ctx))

	// Registering the issuer to the tenant IS the authorization — there is no
	// server-side grant. The token's self-assigned permissions are authoritative,
	// scoped to the issuer's own tenant resources.
	token := mintServiceJWT(t, signer, issuer, subject, []string{PermEntitlementsRead, PermCreditsWrite}, []string{"openrails"})
	resolved, err := cp.ResolveServiceJWT(ctx, token)
	require.NoError(t, err)
	require.Equal(t, fedTenantA, resolved.TenantID)
	require.Equal(t, "tenant-a", resolved.TenantSlug)
	require.ElementsMatch(t, []string{PermEntitlementsRead, PermCreditsWrite}, resolved.Permissions, "self-assigned token permissions are authoritative")
	require.Contains(t, resourceIDs(resolved.Resources, ResourceKindTenant), fedTenantA.String())

	// Authority anchors to the issuer's tenant, not a per-subject grant: any
	// subject minted by a registered issuer is authorized over its own tenant.
	resolvedUnknown, err := cp.ResolveServiceJWT(ctx, mintServiceJWT(t, signer, issuer, "service:unknown", []string{PermEntitlementsRead}, []string{"openrails"}))
	require.NoError(t, err)
	require.Equal(t, fedTenantA, resolvedUnknown.TenantID)

	// Audience verification still applies: an aud not accepted for the registered
	// issuer is rejected.
	_, err = cp.ResolveServiceJWT(ctx, mintServiceJWT(t, signer, issuer, subject, []string{PermEntitlementsRead}, []string{"wrong-audience"}))
	require.Error(t, err)

	// An unregistered issuer resolves to no tenant → denied.
	_, err = cp.ResolveServiceJWT(ctx, mintServiceJWT(t, signer, "https://unregistered-service.example", subject, []string{PermEntitlementsRead}, []string{"openrails"}))
	require.Error(t, err)

	for _, service := range []struct {
		name        string
		subject     string
		permissions []string
	}{
		{name: "hentai0 entitlement read", subject: "service:hentai0-runtime", permissions: []string{PermEntitlementsRead}},
		{name: "tensorhub credits", subject: "service:tensorhub-runtime", permissions: []string{PermCreditsRead, PermCreditsWrite}},
	} {
		t.Run(service.name, func(t *testing.T) {
			resolved, err := cp.ResolveServiceJWT(ctx, mintServiceJWT(t, signer, issuer, service.subject, service.permissions, []string{"openrails"}))
			require.NoError(t, err)
			require.Equal(t, fedTenantA, resolved.TenantID)
			require.ElementsMatch(t, service.permissions, resolved.Permissions)
		})
	}

	_, err = pool.Exec(ctx, `UPDATE billing.tenant_delegated_issuers SET enabled = FALSE WHERE issuer = $1`, issuer)
	require.NoError(t, err)
	require.NoError(t, cp.reloadDelegatedIssuers(ctx))
	_, err = cp.ResolveServiceJWT(ctx, token)
	require.Error(t, err)
}

func TestFederatedServiceJWTClaimRejections(t *testing.T) {
	ctx := context.Background()
	pool := newFedTestPool(t)
	cp := newFedControlPlane(t, pool)

	signer, err := jwtkit.NewRSASigner(2048, "service-claim-kid")
	require.NoError(t, err)
	issuer := "https://service-claims.example"
	subject := "service:hentai0-runtime"
	registerIssuerRow(t, pool, fedTenantA, issuer, jwksServer(t, signer).URL, true)
	require.NoError(t, cp.reloadDelegatedIssuers(ctx))

	now := time.Now().UTC()
	base := jwt.MapClaims{
		"iss":         issuer,
		"sub":         subject,
		"aud":         []string{"openrails"},
		"iat":         now.Unix(),
		"nbf":         now.Add(-time.Second).Unix(),
		"exp":         now.Add(5 * time.Minute).Unix(),
		"jti":         "claim-test",
		"token_use":   authcore.ServiceJWTTokenUse,
		"permissions": []string{PermEntitlementsRead},
	}

	for _, tc := range []struct {
		name   string
		mutate func(jwt.MapClaims)
	}{
		{name: "missing token_use", mutate: func(c jwt.MapClaims) { delete(c, "token_use") }},
		{name: "missing jti", mutate: func(c jwt.MapClaims) { delete(c, "jti") }},
		{name: "missing iat", mutate: func(c jwt.MapClaims) { delete(c, "iat") }},
		{name: "missing nbf", mutate: func(c jwt.MapClaims) { delete(c, "nbf") }},
		{name: "expired", mutate: func(c jwt.MapClaims) { c["exp"] = now.Add(-time.Minute).Unix() }},
		{name: "excessive lifetime", mutate: func(c jwt.MapClaims) { c["exp"] = now.Add(30 * time.Minute).Unix() }},
		{name: "delegated_sub present", mutate: func(c jwt.MapClaims) { c["delegated_sub"] = "user-1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := cloneClaims(base)
			tc.mutate(claims)
			token, err := signer.SignWithHeaders(ctx, claims, map[string]any{"typ": authcore.ServiceJWTType})
			require.NoError(t, err)
			_, err = cp.ResolveServiceJWT(ctx, token)
			require.Error(t, err)
		})
	}
}

func mintServiceJWT(t *testing.T, signer *jwtkit.RSASigner, issuer, subject string, permissions, audiences []string) string {
	t.Helper()
	token, _, err := authcore.MintServiceJWT(context.Background(), signer, issuer, authcore.ServiceJWTMintOptions{
		Subject:     subject,
		Audiences:   audiences,
		Permissions: permissions,
	})
	require.NoError(t, err)
	return token
}

func cloneClaims(in jwt.MapClaims) jwt.MapClaims {
	out := make(jwt.MapClaims, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
		tok := mintFed(t, iss.signer, iss.issuer, "shared-user-1", []string{PermSelfBillingRead}, "tenant-a")
		res, err := cp.ResolveDelegated(ctx, tok)
		require.NoError(t, err, "issuer %s", iss.issuer)
		require.Equal(t, fedTenantA, res.TenantID)
		require.Equal(t, "shared-user-1", res.DelegatedSubject)
		require.Equal(t, iss.issuer, res.Issuer)
	}

	// Tenant B's issuer resolves to B (distinct).
	tokB := mintFed(t, sgB, issB, "b-user", []string{PermSelfBillingRead}, "tenant-b")
	resB, err := cp.ResolveDelegated(ctx, tokB)
	require.NoError(t, err)
	require.Equal(t, fedTenantB, resB.TenantID)

	// CROSS-TENANT: tenant A's issuer naming tenant B in its `tenant` claim is
	// rejected — a tenant-signed token can never assert a tenant other than the
	// one its (globally-unique) issuer is pinned to.
	forged := mintFed(t, sgA1, issA1, "evil", []string{PermSelfBillingRead}, "tenant-b")
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

	tokA1 := mintFed(t, sgA1, issA1, "u", []string{PermSelfBillingRead}, "tenant-a")
	tokA2 := mintFed(t, sgA2, issA2, "u", []string{PermSelfBillingRead}, "tenant-a")
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
	tok := mintFed(t, sg, iss, "admin-user", []string{PermTenantBillingRead, PermTenantEntitlementsWrite}, "tenant-a")
	res, err := cp.ResolveDelegated(ctx, tok)
	require.NoError(t, err)
	require.Equal(t, fedTenantA, res.TenantID)
	require.Equal(t, "admin-user", res.DelegatedSubject)
	require.True(t, res.HasPermission(PermTenantEntitlementsWrite))
}

func federatedWrongAudience(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	cp := newFedControlPlane(t, pool)

	sg, _ := jwtkit.NewRSASigner(2048, "wrong-aud-kid")
	iss := "https://wrong-aud.test"
	registerIssuerRow(t, pool, fedTenantA, iss, jwksServer(t, sg).URL, true)
	require.NoError(t, cp.reloadDelegatedIssuers(ctx))

	tok := mintFedWithAudience(t, sg, iss, "u", []string{PermSelfBillingRead}, "tenant-a", []string{"tensorhub"})
	_, err := cp.ResolveDelegated(ctx, tok)
	require.ErrorIs(t, err, ErrDelegatedInvalid)
}

func federatedUnregistered(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	cp := newFedControlPlane(t, pool)

	// A perfectly-valid signer whose issuer was never registered. Its key is not
	// in the verifier, so it fails closed.
	sg, _ := jwtkit.NewRSASigner(2048, "ghost-kid")
	tok := mintFed(t, sg, "https://ghost.test", "u", []string{PermSelfBillingRead}, "tenant-a")
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
		TenantID: fedTenantA, Issuer: iss, JWKSURI: js, Audiences: []string{"openrails"},
	}))
	oldToken := mintFed(t, sg, iss, "rotated-user", []string{PermSelfBillingRead}, "tenant-a")
	_, err := cp.ResolveDelegated(ctx, oldToken)
	require.NoError(t, err)

	// Tenant B cannot claim the same globally-unique issuer.
	err = cp.RegisterDelegatedIssuer(ctx, RegisterDelegatedIssuerParams{
		TenantID: fedTenantB, Issuer: iss, JWKSURI: js, Audiences: []string{"openrails"},
	})
	require.ErrorIs(t, err, ErrIssuerOwnedByOtherTenant)

	// Tenant A rotating its own issuer (same issuer, new JWKS) succeeds.
	sg2, _ := jwtkit.NewRSASigner(2048, "k2")
	require.NoError(t, cp.RegisterDelegatedIssuer(ctx, RegisterDelegatedIssuerParams{
		TenantID: fedTenantA, Issuer: iss, JWKSURI: jwksServer(t, sg2).URL, Audiences: []string{"openrails"},
	}))
	_, err = cp.ResolveDelegated(ctx, oldToken)
	require.ErrorIs(t, err, ErrDelegatedInvalid)
	newToken := mintFed(t, sg2, iss, "rotated-user", []string{PermSelfBillingRead}, "tenant-a")
	_, err = cp.ResolveDelegated(ctx, newToken)
	require.NoError(t, err)
}
