//go:build integration

package bootstrap

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestReconcileMerchantManifestStoresSolanaPSPConfig(t *testing.T) {
	ctx := context.Background()
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	cfg := &config.Config{SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{
		MasterKey: base64.StdEncoding.EncodeToString(key),
	}}
	manifest := hostThreeMerchantManifest()
	mt := manifest.Merchants["host-three"]
	const (
		accountID       = "AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9"
		recipientWallet = "9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu"
		privateKey      = "2AXDGYSE4f2sz7tvMMzyHvUfcoJmxudvdhBcmiUSo6iuCXagjUCKEQF21awZnUGxmwD4m9vGXuC3qieHXJQHAcT"
	)
	mt.PSPs = map[string]PSPConfig{
		"solana": {
			"solana": {
				Signer: &PSPSignerConfig{Mode: "local_keypair"},
				Settings: map[string]any{
					"recipient_wallet": recipientWallet,
				},
				Secrets: map[string]string{
					"private_key": privateKey,
				},
			},
		},
	}
	manifest.Merchants["host-three"] = mt

	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, MerchantManifestReconcileOptions{Insert: true}))

	var merchantID string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id::text FROM billing.merchants WHERE slug = 'host-three'`).Scan(&merchantID))

	var evidenceBytes []byte
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT evidence
		FROM billing.psps
		WHERE merchant_id = $1::uuid AND rail = 'solana' AND environment = 'live' AND account_id = $2
	`, merchantID, accountID).Scan(&evidenceBytes))
	var evidence map[string]any
	require.NoError(t, json.Unmarshal(evidenceBytes, &evidence))
	require.Equal(t, map[string]any{"mode": "local_keypair"}, evidence["signer"])
	require.Equal(t, map[string]any{"recipient_wallet": recipientWallet}, evidence["settings"])

	secretName, err := merchants.PSPSecretName("solana", "live", accountID, "private_key")
	require.NoError(t, err)
	backend, err := merchantsecrets.Build(ctx, cfg, cp.Pool())
	require.NoError(t, err)
	tid, err := merchant.ParseID(merchantID)
	require.NoError(t, err)
	sec, err := backend.Secrets.Get(ctx, tid, secretName)
	require.NoError(t, err)
	require.Equal(t, privateKey, sec.Value)
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

// Keep a fresh database per test: these manifests deliberately reuse merchant
// names and provider identities. Reuse dbtest's owned server and apply exactly
// the production schema instead of reconstructing a subset of its tables.
func newMerchantManifestTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	adminDSN := dbtest.SharedSuperuserDSN(t)
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)
	t.Cleanup(adminPool.Close)

	dbName := fmt.Sprintf("openrails_tenant_manifest_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := adminPool.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{dbName}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err, "clean up this test's database only")
	})

	targetDSN := merchantManifestDatabaseDSN(t, adminDSN, dbName)
	require.NoError(t, migrate.RunPostgres(ctx, &config.Config{
		DB: &config.DBConfig{URL: targetDSN},
	}))
	pool, err := pgxpool.New(ctx, targetDSN)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	dbtest.ApplyAuthKitMigrations(t, ctx, pool, "profiles")
	return pool
}

// ConnString preserves the original DSN when pgx.Config.Database changes.
// Rewrite both URL paths and overriding query parameters before migration.
func merchantManifestDatabaseDSN(t *testing.T, adminDSN, dbName string) string {
	t.Helper()
	targetDSN := adminDSN + " dbname=" + dbName
	if strings.HasPrefix(adminDSN, "postgres://") || strings.HasPrefix(adminDSN, "postgresql://") {
		targetURL, err := url.Parse(adminDSN)
		require.NoError(t, err)
		targetURL.Path = "/" + dbName
		query := targetURL.Query()
		query.Del("dbname")
		targetURL.RawQuery = query.Encode()
		targetDSN = targetURL.String()
	}
	parsed, err := pgxpool.ParseConfig(targetDSN)
	require.NoError(t, err)
	require.Equal(t, dbName, parsed.ConnConfig.Database, "migrations must target only this test's database")
	return targetDSN
}

func TestMerchantManifestDatabaseDSNIsOwned(t *testing.T) {
	for name, original := range map[string]string{
		"url":                "postgres://test:secret@localhost/original?sslmode=disable",
		"url query override": "postgresql://test:secret@localhost/original?sslmode=disable&dbname=foreign&dbname=another",
		"keyword":            "host=localhost user=test password='test secret' dbname=original sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			dsn := merchantManifestDatabaseDSN(t, original, "owned_fixture")
			parsed, err := pgxpool.ParseConfig(dsn)
			require.NoError(t, err)
			require.Equal(t, "owned_fixture", parsed.ConnConfig.Database)
		})
	}
}

// apiModeReconcileConfig pins these store-semantics tests to MODE 2 (#723
// merchant_config_source=api): they assert persistent-backend side effects (seed-once,
// merchant_secrets rows, Vault KV) that mode 1 deliberately does not produce.
// SEC-18: Env is declared, never inferred — an empty Env is no longer
// development, and the DB secret store refuses a plaintext posture outside it.
func apiModeReconcileConfig() *config.Config {
	return &config.Config{SecretBackend: config.SecretBackendSnapshot}
}

// sandboxModeReconcileConfig is apiModeReconcileConfig under test_mode=sandbox.
// #882: that posture is the ONLY thing that puts a PSP in the test environment,
// so a test asserting `psps/<rail>/test/…` must declare it.
func sandboxModeReconcileConfig() *config.Config {
	cfg := apiModeReconcileConfig()
	cfg.TestMode = config.CredentialPostureSandbox
	return cfg
}

func newMerchantManifestControlPlane(t *testing.T, pool *pgxpool.Pool) *controlplane.ControlPlane {
	t.Helper()
	cfg := &config.Config{
		// MintDisabled: "test" is not a dev-like env (#748: verify-only must be
		// declared outside development), and this control plane is never asked
		// to mint in these manifest-reconcile tests.
		Auth: &config.AuthConfig{Issuer: "https://openrails.test", MintDisabled: true, DirectPeerIP: true},
	}
	rdb, _ := dbtest.SharedRedisClient(t)
	cp, err := controlplane.New(context.Background(), cfg, pool, controlplane.WithRedis(rdb))
	require.NoError(t, err)
	t.Cleanup(cp.Close)
	require.NotNil(t, cp)
	return cp
}

func hostThreeMerchantManifest() *BillingConfig {
	return &BillingConfig{
		Version: BootstrapManifestVersion,
		Merchants: map[string]MerchantConfig{
			"host-three": {
				DisplayName: "Host Three",
			},
		},
	}
}
