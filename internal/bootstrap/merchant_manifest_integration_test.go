//go:build integration

package bootstrap

import (
	"context"
	"fmt"
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
	authpgmigrations "github.com/open-rails/authkit/migrations/postgres"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
)

const merchantManifestSchemaDDL = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.merchants (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'active',
    owner_org_id     TEXT,
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

CREATE TABLE IF NOT EXISTS openrails.merchant_configurations (
    merchant_id uuid NOT NULL,
    config jsonb DEFAULT '{}'::jsonb NOT NULL,
    config_version bigint DEFAULT 1 NOT NULL,
    created_at timestamptz DEFAULT now() NOT NULL,
    updated_at timestamptz DEFAULT now() NOT NULL,
    CONSTRAINT merchant_configurations_pkey PRIMARY KEY (merchant_id),
    CONSTRAINT merchant_configurations_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id)
);
ALTER TABLE ONLY openrails.merchant_configurations FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.merchant_configurations ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.merchant_configurations;
CREATE POLICY merchant_isolation ON openrails.merchant_configurations
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));

CREATE TABLE IF NOT EXISTS openrails.merchant_secrets (
    merchant_id uuid NOT NULL,
    name text NOT NULL,
    value text NOT NULL,
    version integer DEFAULT 1 NOT NULL,
    created_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT pk_merchant_secrets PRIMARY KEY (merchant_id, name)
);
ALTER TABLE ONLY openrails.merchant_secrets FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.merchant_secrets ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.merchant_secrets;
CREATE POLICY merchant_isolation ON openrails.merchant_secrets
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));

CREATE TABLE IF NOT EXISTS openrails.provider_accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    provider_type text NOT NULL,
    account_id text NOT NULL,
    display_name text,
    vault_secret_ref text,
    role text DEFAULT 'primary' NOT NULL,
    status text DEFAULT 'enabled' NOT NULL,
    evidence jsonb,
    first_seen_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    last_verified_at timestamptz,
    replaced_at timestamptz,
    created_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT provider_accounts_pkey PRIMARY KEY (id),
    CONSTRAINT provider_accounts_nonempty CHECK (btrim(provider_type) <> '' AND btrim(account_id) <> ''),
    CONSTRAINT provider_accounts_role_check CHECK (role = ANY (ARRAY['primary','secondary','legacy'])),
    CONSTRAINT provider_accounts_status_check CHECK (status = ANY (ARRAY['enabled','disabled'])),
    CONSTRAINT provider_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE
);
ALTER TABLE ONLY openrails.provider_accounts FORCE ROW LEVEL SECURITY;
CREATE UNIQUE INDEX IF NOT EXISTS uq_provider_accounts_identity ON openrails.provider_accounts (merchant_id, provider_type, account_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_provider_accounts_enabled_primary ON openrails.provider_accounts (merchant_id, provider_type) WHERE (role = 'primary' AND status = 'enabled');
ALTER TABLE openrails.provider_accounts ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.provider_accounts;
CREATE POLICY merchant_isolation ON openrails.provider_accounts
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));
`

func TestReconcileMerchantManifestEnsuresTenants(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, cozyArtMerchantManifest("starter", "us-west", "/hooks/v1"), MerchantManifestReconcileOptions{}))

	var tenantID, ownerOrgID, billingTier, region, webhookPath string
	require.NoError(t, pool.QueryRow(ctx, `
			SELECT id::text, owner_org_id, billing_tier, region, webhook_path
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&tenantID, &ownerOrgID, &billingTier, &region, &webhookPath))
	require.Equal(t, "starter", billingTier)
	require.Equal(t, "us-west", region)
	require.Equal(t, "/hooks/v1", webhookPath)

	ownerOrg, err := cp.Core().ResolveOrgBySlug(ctx, "cozy-art")
	require.NoError(t, err)
	require.Equal(t, ownerOrg.ID, ownerOrgID, "manifest bootstrap should bind the merchant namespace to a bootstrap-created owner org")

	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, cozyArtMerchantManifest("pro", "us-east", "/hooks/v2"), MerchantManifestReconcileOptions{}))

	require.NoError(t, pool.QueryRow(ctx, `
			SELECT owner_org_id, billing_tier, region, webhook_path
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&ownerOrgID, &billingTier, &region, &webhookPath))
	require.Equal(t, ownerOrg.ID, ownerOrgID)
	require.Equal(t, "pro", billingTier)
	require.Equal(t, "us-east", region)
	require.Equal(t, "/hooks/v2", webhookPath)
}

func TestReconcileMerchantManifestAppliesMerchantConfiguration(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest("starter", "us-west", "/hooks/v1")
	manifest.Merchants[0].Profile = ManifestMerchantProfile{
		DisplayName:  "Cozy Art Billing",
		LogoURL:      "https://cdn.example/logo.png",
		FromEmail:    "billing@example.com",
		SupportURL:   "https://example.com/support",
		SupportEmail: "support@example.com",
	}
	manifest.Merchants[0].ProviderAccounts = []ManifestProviderAccount{{
		ProviderType: "stripe",
		AccountID:    "acct_test_123",
		DisplayName:  "Stripe primary",
		Role:         "primary",
		Secrets: map[string]ManifestSecretSource{
			"secret_key": {Value: "sk_test_bootstrap"},
		},
	}}

	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, manifest, MerchantManifestReconcileOptions{}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))

	var displayName, logoURL, fromEmail, supportURL, supportEmail string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT
			config #>> '{profile,display_name}',
			config #>> '{profile,logo_url}',
			config #>> '{profile,from_email}',
			config #>> '{profile,support_url}',
			config #>> '{profile,support_email}'
		FROM openrails.merchant_configurations
		WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&displayName, &logoURL, &fromEmail, &supportURL, &supportEmail))
	require.Equal(t, "Cozy Art Billing", displayName)
	require.Equal(t, "https://cdn.example/logo.png", logoURL)
	require.Equal(t, "billing@example.com", fromEmail)
	require.Equal(t, "https://example.com/support", supportURL)
	require.Equal(t, "support@example.com", supportEmail)

	var secretValue string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = 'stripe/secret_key'
	`, merchantID).Scan(&secretValue))
	require.Equal(t, "sk_test_bootstrap", secretValue)

	var providerType, accountID, display, role, status string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT provider_type, account_id, display_name, role, status
		FROM openrails.provider_accounts
		WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&providerType, &accountID, &display, &role, &status))
	require.Equal(t, "stripe", providerType)
	require.Equal(t, "acct_test_123", accountID)
	require.Equal(t, "Stripe primary", display)
	require.Equal(t, "primary", role)
	require.Equal(t, "enabled", status)
}

func TestReconcileMerchantManifestSerializesConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest("starter", "us-west", "/hooks/v1")

	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := ReconcileMerchantManifestData(ctx, &config.Config{}, cp, manifest, MerchantManifestReconcileOptions{}); err == nil {
				successes.Add(1)
			} else {
				require.NoError(t, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.EqualValues(t, 2, successes.Load())

}

func newMerchantManifestTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalMerchantManifestTestPool(t, ctx, dsn)
		applyMerchantManifestTestSchema(t, ctx, pool)
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

	applyMerchantManifestTestSchema(t, ctx, pool)
	return pool
}

func newExternalMerchantManifestTestPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
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

func applyMerchantManifestTestSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
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
	_, err = pool.Exec(ctx, merchantManifestSchemaDDL)
	require.NoError(t, err)
}

func newMerchantManifestControlPlane(t *testing.T, pool *pgxpool.Pool) *controlplane.ControlPlane {
	t.Helper()
	cfg := &config.Config{
		Env:  "test",
		Auth: &config.AuthConfig{Issuer: "https://openrails.test"},
	}
	cp, err := controlplane.New(context.Background(), cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, cp)
	return cp
}

func cozyArtMerchantManifest(tier, region, webhookPath string) *MerchantManifest {
	return &MerchantManifest{
		Version: BootstrapManifestVersion,
		Merchants: []ManifestMerchant{{
			Slug:        "cozy-art",
			Name:        "Cozy Art",
			BillingTier: tier,
			Region:      region,
			WebhookHost: "cozy.example",
			WebhookPath: webhookPath,
		}},
	}
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
