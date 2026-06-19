//go:build integration

package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
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
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/pkg/merchant"
)

const merchantManifestSchemaDDL = `
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.merchants (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug                TEXT NOT NULL UNIQUE,
    status              TEXT NOT NULL DEFAULT 'active',
    owner_org_id     TEXT,
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

CREATE TABLE IF NOT EXISTS openrails.merchant_deks (
    merchant_id uuid NOT NULL,
    wrapped_dek bytea NOT NULL,
    key_version integer DEFAULT 1 NOT NULL,
    created_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamptz DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT pk_merchant_deks PRIMARY KEY (merchant_id)
);
ALTER TABLE ONLY openrails.merchant_deks FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.merchant_deks ENABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS merchant_isolation ON openrails.merchant_deks;
CREATE POLICY merchant_isolation ON openrails.merchant_deks
    USING ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid))
    WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id', true), ''))::uuid));

CREATE TABLE IF NOT EXISTS openrails.provider_accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    provider_type text NOT NULL,
    environment text DEFAULT 'live' NOT NULL,
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
    CONSTRAINT provider_accounts_nonempty CHECK (btrim(provider_type) <> '' AND btrim(environment) <> '' AND btrim(account_id) <> ''),
    CONSTRAINT provider_accounts_environment_check CHECK (environment = ANY (ARRAY['live','test'])),
    CONSTRAINT provider_accounts_role_check CHECK (role = ANY (ARRAY['primary','secondary','legacy'])),
    CONSTRAINT provider_accounts_status_check CHECK (status = ANY (ARRAY['enabled','disabled'])),
    CONSTRAINT provider_accounts_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE
);
ALTER TABLE ONLY openrails.provider_accounts FORCE ROW LEVEL SECURITY;
CREATE UNIQUE INDEX IF NOT EXISTS uq_provider_accounts_identity ON openrails.provider_accounts (merchant_id, provider_type, environment, account_id);
CREATE UNIQUE INDEX IF NOT EXISTS uq_provider_accounts_enabled_primary ON openrails.provider_accounts (merchant_id, provider_type, environment) WHERE (role = 'primary' AND status = 'enabled');
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
	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, cozyArtMerchantManifest(), MerchantManifestReconcileOptions{Insert: true}))

	var tenantID, ownerOrgID string
	require.NoError(t, pool.QueryRow(ctx, `
			SELECT id::text, owner_org_id
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&tenantID, &ownerOrgID))

	ownerOrg, err := cp.Core().ResolveOrgBySlug(ctx, "cozy-art")
	require.NoError(t, err)
	require.Equal(t, ownerOrg.ID, ownerOrgID, "manifest bootstrap should bind the merchant namespace to a bootstrap-created owner org")

	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, cozyArtMerchantManifest(), MerchantManifestReconcileOptions{Insert: true}))

	require.NoError(t, pool.QueryRow(ctx, `
			SELECT owner_org_id
		  FROM openrails.merchants
		 WHERE slug = 'cozy-art'
	`).Scan(&ownerOrgID))
	require.Equal(t, ownerOrg.ID, ownerOrgID)
}

func TestReconcileMerchantManifestAppliesMerchantConfiguration(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()
	manifest.Merchants[0].Profile = ManifestMerchantProfile{
		DisplayName: "Cozy Art Billing",
		LogoURL:     "https://cdn.example/logo.png",
		FromEmail:   "billing@example.com",
		SupportURL:  "https://example.com/support",
	}
	manifest.Merchants[0].ProviderAccounts = []ManifestProviderAccount{{
		ProviderType: "stripe",
		Environment:  "test",
		AccountID:    "acct_test_123",
		Mode:         "primary",
		Secrets: map[string]ManifestSecretSource{
			"secret_key": {Value: "sk_test_bootstrap"},
		},
	}}

	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))

	var displayName, logoURL, fromEmail, supportURL string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT
			config #>> '{profile,display_name}',
			config #>> '{profile,logo_url}',
			config #>> '{profile,from_email}',
			config #>> '{profile,support_url}'
		FROM openrails.merchant_configurations
		WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&displayName, &logoURL, &fromEmail, &supportURL))
	require.Equal(t, "Cozy Art Billing", displayName)
	require.Equal(t, "https://cdn.example/logo.png", logoURL)
	require.Equal(t, "billing@example.com", fromEmail)
	require.Equal(t, "https://example.com/support", supportURL)

	var secretValue string
	secretName, err := merchants.ProviderAccountSecretName("stripe", "test", "acct_test_123", "secret_key")
	require.NoError(t, err)
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = $2
	`, merchantID, secretName).Scan(&secretValue))
	require.Equal(t, "sk_test_bootstrap", secretValue)

	var providerType, environment, accountID, role, status string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT provider_type, environment, account_id, role, status
		FROM openrails.provider_accounts
		WHERE merchant_id = $1::uuid
	`, merchantID).Scan(&providerType, &environment, &accountID, &role, &status))
	require.Equal(t, "stripe", providerType)
	require.Equal(t, "test", environment)
	require.Equal(t, "acct_test_123", accountID)
	require.Equal(t, "primary", role)
	require.Equal(t, "enabled", status)
}

func TestReconcileMerchantManifestDiscoversProviderAccountIdentityFromSecrets(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()
	manifest.Merchants[0].ProviderAccounts = []ManifestProviderAccount{{
		ProviderType: "ccbill",
		Environment:  "live",
		Mode:         "primary",
		Secrets: map[string]ManifestSecretSource{
			"account_config": {Value: `{"client_acc_num":"900000","client_sub_acc":"0000","salt":"secret"}`},
		},
	}}

	require.NoError(t, ReconcileMerchantManifestData(ctx, &config.Config{}, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantID))
	var accountID string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT account_id
		FROM openrails.provider_accounts
		WHERE merchant_id = $1::uuid AND provider_type = 'ccbill'
	`, merchantID).Scan(&accountID))
	require.Equal(t, "900000/0000", accountID)

	secretName, err := merchants.ProviderAccountSecretName("ccbill", "live", "900000/0000", "account_config")
	require.NoError(t, err)
	var secretValue string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = $2
	`, merchantID, secretName).Scan(&secretValue))
	require.JSONEq(t, `{"client_acc_num":"900000","client_sub_acc":"0000","salt":"secret"}`, secretValue)
}

func TestReconcileMerchantManifestUsesConfiguredVaultSecretBackend(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	vault := newMerchantManifestVault(t)
	cfg := &config.Config{Vault: &config.VaultConfig{
		Enabled:    true,
		Address:    vault.Address,
		AuthMethod: "token",
		Token:      vault.Token,
	}}

	manifest := cozyArtMerchantManifest()
	manifest.Merchants[0].ProviderAccounts = []ManifestProviderAccount{{
		ProviderType: "stripe",
		Environment:  "test",
		AccountID:    "acct_vault_123",
		Mode:         "primary",
		Secrets: map[string]ManifestSecretSource{
			"secret_key": {Value: "sk_test_vault_bootstrap"},
		},
	}}

	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantIDText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantIDText))
	secretName, err := merchants.ProviderAccountSecretName("stripe", "test", "acct_vault_123", "secret_key")
	require.NoError(t, err)
	vaultPath := "secret/openrails/merchants/cozy-art/" + secretName
	require.Equal(t, "sk_test_vault_bootstrap", readVaultKV2Value(t, vault, vaultPath))

	var dbSecretCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = $2
	`, merchantIDText, secretName).Scan(&dbSecretCount))
	require.Equal(t, 0, dbSecretCount, "Vault-enabled bootstrap must not import provider secrets into DB merchant_secrets")

	backend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	require.NoError(t, err)
	merchantID, err := merchant.ParseID(merchantIDText)
	require.NoError(t, err)
	sec, err := backend.Secrets.Get(ctx, merchantID, secretName)
	require.NoError(t, err)
	require.Equal(t, "sk_test_vault_bootstrap", sec.Value)
}

func TestReconcileMerchantManifestUsesEncryptedDBSecretBackend(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cfg := &config.Config{Encryption: &config.EncryptionConfig{
		MasterKey: base64.StdEncoding.EncodeToString(key),
	}}

	manifest := cozyArtMerchantManifest()
	manifest.Merchants[0].ProviderAccounts = []ManifestProviderAccount{{
		ProviderType: "stripe",
		Environment:  "test",
		AccountID:    "acct_db_123",
		Mode:         "primary",
		Secrets: map[string]ManifestSecretSource{
			"secret_key": {Value: "sk_test_db_bootstrap"},
		},
	}}

	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantIDText string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE slug = 'cozy-art'`).Scan(&merchantIDText))
	secretName, err := merchants.ProviderAccountSecretName("stripe", "test", "acct_db_123", "secret_key")
	require.NoError(t, err)

	var storedValue string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value
		FROM openrails.merchant_secrets
		WHERE merchant_id = $1::uuid AND name = $2
	`, merchantIDText, secretName).Scan(&storedValue))
	require.NotEqual(t, "sk_test_db_bootstrap", storedValue, "DB-backed bootstrap must store encrypted ciphertext when encryption is configured")

	backend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	require.NoError(t, err)
	merchantID, err := merchant.ParseID(merchantIDText)
	require.NoError(t, err)
	sec, err := backend.Secrets.Get(ctx, merchantID, secretName)
	require.NoError(t, err)
	require.Equal(t, "sk_test_db_bootstrap", sec.Value)
}

func TestReconcileMerchantManifestSerializesConcurrentReplicas(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	manifest := cozyArtMerchantManifest()

	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := ReconcileMerchantManifestData(ctx, &config.Config{}, cp, manifest, MerchantManifestReconcileOptions{Insert: true}); err == nil {
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

type merchantManifestVault struct {
	Address string
	Token   string
}

func newMerchantManifestVault(t *testing.T) merchantManifestVault {
	t.Helper()
	ctx := context.Background()
	const token = "root"
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "hashicorp/vault:1.19",
			ExposedPorts: []string{"8200/tcp"},
			Env: map[string]string{
				"VAULT_DEV_ROOT_TOKEN_ID": token,
			},
			Cmd: []string{"server", "-dev", "-dev-root-token-id=" + token, "-dev-listen-address=0.0.0.0:8200"},
			WaitingFor: wait.ForHTTP("/v1/sys/health").
				WithPort("8200/tcp").
				WithStatusCodeMatcher(func(status int) bool {
					return status == http.StatusOK || status == http.StatusTooManyRequests
				}).
				WithStartupTimeout(60 * time.Second),
		},
		Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "8200/tcp")
	require.NoError(t, err)
	return merchantManifestVault{
		Address: "http://" + net.JoinHostPort(host, port.Port()),
		Token:   token,
	}
}

func readVaultKV2Value(t *testing.T, vault merchantManifestVault, fullPath string) string {
	t.Helper()
	rest := strings.TrimPrefix(strings.TrimPrefix(fullPath, "secret"), "/")
	req, err := http.NewRequest(http.MethodGet, vault.Address+"/v1/secret/data/"+rest, nil)
	require.NoError(t, err)
	req.Header.Set("X-Vault-Token", vault.Token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var payload struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&payload))
	return payload.Data.Data["value"]
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

func cozyArtMerchantManifest() *MerchantManifest {
	return &MerchantManifest{
		Version: BootstrapManifestVersion,
		Merchants: []ManifestMerchant{{
			Slug:        "cozy-art",
			DisplayName: "Cozy Art",
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
