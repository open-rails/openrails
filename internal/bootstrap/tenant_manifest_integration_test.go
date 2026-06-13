//go:build integration

package bootstrap

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/core"
	jwtkit "github.com/open-rails/authkit/jwt"
	authpgmigrations "github.com/open-rails/authkit/migrations/postgres"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
)

const tenantManifestSchemaDDL = `
CREATE SCHEMA IF NOT EXISTS billing;

CREATE TABLE IF NOT EXISTS openrails.tenants (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active',
    authkit_tenant_id   TEXT,
    authkit_tenant_slug TEXT,
    plan                TEXT,
    region              TEXT,
    billing_tier        TEXT,
    stripe_account_id   TEXT,
    webhook_host        TEXT,
    webhook_path        TEXT,
    provisioned_at      TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    suspended_at        TIMESTAMPTZ,
    deleted_at          TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS openrails.tenant_delegated_issuers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID NOT NULL REFERENCES openrails.tenants (id) ON DELETE CASCADE,
    issuer      TEXT NOT NULL,
    jwks_uri    TEXT NOT NULL,
    audiences   TEXT[] NOT NULL DEFAULT '{}',
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    CONSTRAINT tenant_delegated_issuers_issuer_unique UNIQUE (issuer)
);
`

func TestReconcileTenantManifestEnsuresTenantsAndIssuers(t *testing.T) {
	ctx := context.Background()
	pool := newTenantManifestTestPool(t)
	cp := newTenantManifestControlPlane(t, pool)
	require.NoError(t, ReconcileTenantManifestData(ctx, &config.Config{}, cp, cozyArtTenantManifest("starter", "us-west", "/hooks/v1"), TenantManifestReconcileOptions{}))

	var tenantID, billingTier, region, webhookPath, authkitTenantSlug string
	require.NoError(t, pool.QueryRow(ctx, `
			SELECT id::text, billing_tier, region, webhook_path, authkit_tenant_slug
		  FROM openrails.tenants
		 WHERE slug = 'cozy-art'
	`).Scan(&tenantID, &billingTier, &region, &webhookPath, &authkitTenantSlug))
	require.Equal(t, "starter", billingTier)
	require.Equal(t, "us-west", region)
	require.Equal(t, "/hooks/v1", webhookPath)
	require.Equal(t, "tenant-cozy-art", authkitTenantSlug)

	serviceTokens, err := cp.Core().ListServiceTokens(ctx, "tenant-cozy-art")
	require.NoError(t, err)
	require.Empty(t, serviceTokens, "bootstrap manifests must not mint generated service tokens")

	require.NoError(t, ReconcileTenantManifestData(ctx, &config.Config{}, cp, cozyArtTenantManifest("pro", "us-east", "/hooks/v2"), TenantManifestReconcileOptions{}))

	require.NoError(t, pool.QueryRow(ctx, `
			SELECT billing_tier, region, webhook_path
		  FROM openrails.tenants
		 WHERE slug = 'cozy-art'
	`).Scan(&billingTier, &region, &webhookPath))
	require.Equal(t, "pro", billingTier)
	require.Equal(t, "us-east", region)
	require.Equal(t, "/hooks/v2", webhookPath)

	serviceTokens, err = cp.Core().ListServiceTokens(ctx, "tenant-cozy-art")
	require.NoError(t, err)
	require.Empty(t, serviceTokens, "idempotent reconcile must not mint generated service tokens")

	issuer1 := newJWKS(t, "issuer-kid-1")
	issuer2 := newJWKS(t, "issuer-kid-2")
	issuerManifest := &TenantManifest{Version: 1, Tenants: []ManifestTenant{{
		Slug: "cozy-art",
		Issuers: []ManifestIssuer{{
			Issuer:    "https://doujins.example",
			JWKSURI:   issuer1.URL,
			Audiences: []string{"openrails-v1"},
		}},
	}}}
	require.True(t, reconcileManifestIssuersOnce(ctx, cp, issuerManifest))
	assertIssuerRow(t, pool, "https://doujins.example", issuer1.URL, []string{"openrails-v1"}, true)

	issuerManifest.Tenants[0].Issuers[0].JWKSURI = issuer2.URL
	issuerManifest.Tenants[0].Issuers[0].Audiences = []string{"openrails-v2", "openrails-admin"}
	require.True(t, reconcileManifestIssuersOnce(ctx, cp, issuerManifest))
	assertIssuerRow(t, pool, "https://doujins.example", issuer2.URL, []string{"openrails-v2", "openrails-admin"}, true)

	disabled := false
	issuerManifest.Tenants[0].Issuers[0].Enabled = &disabled
	require.True(t, reconcileManifestIssuersOnce(ctx, cp, issuerManifest))
	assertIssuerRow(t, pool, "https://doujins.example", issuer2.URL, []string{"openrails-v2", "openrails-admin"}, false)
}

func TestReconcileTenantManifestSerializesConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	pool := newTenantManifestTestPool(t)
	cp := newTenantManifestControlPlane(t, pool)
	manifest := cozyArtTenantManifest("starter", "us-west", "/hooks/v1")

	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := ReconcileTenantManifestData(ctx, &config.Config{}, cp, manifest, TenantManifestReconcileOptions{}); err == nil {
				successes.Add(1)
			} else {
				require.NoError(t, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 2, successes.Load())

	serviceTokens, err := cp.Core().ListServiceTokens(ctx, "tenant-cozy-art")
	require.NoError(t, err)
	require.Empty(t, serviceTokens, "bootstrap manifests must not mint generated service tokens")
}

func newTenantManifestTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalTenantManifestTestPool(t, ctx, dsn)
		applyTenantManifestTestSchema(t, ctx, pool)
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

	applyTenantManifestTestSchema(t, ctx, pool)
	return pool
}

func newExternalTenantManifestTestPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("openrails_tenant_manifest_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
	require.NoError(t, err)

	testCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	testCfg.ConnConfig.Config.Database = dbName
	pool, err := pgxpool.NewWithConfig(ctx, testCfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", dbName)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize())
		adminPool.Close()
	})
	return pool
}

func applyTenantManifestTestSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	entries, err := authpgmigrations.FS.ReadDir(".")
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		b, rerr := authpgmigrations.FS.ReadFile(name)
		require.NoError(t, rerr)
		_, eerr := pool.Exec(ctx, string(b))
		require.NoErrorf(t, eerr, "apply authkit migration %s", name)
	}
	_, err = pool.Exec(ctx, tenantManifestSchemaDDL)
	require.NoError(t, err)
}

func newTenantManifestControlPlane(t *testing.T, pool *pgxpool.Pool) *controlplane.ControlPlane {
	t.Helper()
	cfg := &config.Config{
		Env: "test",
		Auth: &config.AuthConfig{
			ExpectedAudience: "openrails",
			ControlPlane: &config.ControlPlaneConfig{
				Issuer:      "https://openrails.test",
				TokenPrefix: "openrails",
			},
		},
	}
	cp, err := controlplane.New(context.Background(), cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)
	return cp
}

func cozyArtTenantManifest(tier, region, webhookPath string) *TenantManifest {
	return &TenantManifest{
		Version: BootstrapManifestVersion,
		Tenants: []ManifestTenant{{
			Slug:        "cozy-art",
			Name:        "Cozy Art",
			BillingTier: tier,
			Region:      region,
			WebhookHost: "cozy.example",
			WebhookPath: webhookPath,
		}},
	}
}

func newJWKS(t *testing.T, kid string) *httptest.Server {
	t.Helper()
	signer, err := jwtkit.NewRSASigner(2048, kid)
	require.NoError(t, err)
	keys := jwtkit.JWKS{Keys: []jwtkit.JWK{
		jwtkit.PublicToJWK(signer.PublicKey(), signer.KID(), "RS256"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jwtkit.ServeJWKS(w, r, keys)
	}))
	t.Cleanup(srv.Close)
	return srv
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

func assertIssuerRow(t *testing.T, pool *pgxpool.Pool, issuer, jwksURI string, audiences []string, enabled bool) {
	t.Helper()
	var (
		gotJWKS      string
		gotAudiences []string
		gotEnabled   bool
	)
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT jwks_uri, audiences, enabled
		  FROM openrails.tenant_delegated_issuers
		 WHERE issuer = $1
	`, issuer).Scan(&gotJWKS, &gotAudiences, &gotEnabled))
	require.Equal(t, jwksURI, gotJWKS)
	require.ElementsMatch(t, audiences, gotAudiences)
	require.Equal(t, enabled, gotEnabled)
}
