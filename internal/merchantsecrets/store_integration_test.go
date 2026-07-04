//go:build integration

package merchantsecrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
	"github.com/open-rails/openrails/internal/merchants"
)

// TestMain composes the dbtest and vaulttest shared-container teardowns (both
// RunMain variants os.Exit, so they cannot nest).
func TestMain(m *testing.M) {
	code := m.Run()
	vaulttest.TerminateShared()
	dbtest.TerminateShared()
	dbtest.TerminateSharedRedis()
	os.Exit(code)
}

func startSecretsPostgres(t *testing.T) (*db.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	rawPool, err := pgxpool.New(ctx, dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	t.Cleanup(rawPool.Close)
	return db.WrapPool(rawPool, config.DefaultSchema), ctx
}

func testMasterKey(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, k)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(k)
}

// vaultBackedConfig is a mode-2 config (#723 merchant_source=api): secrets
// declared to live in Vault, reached with the given token (#724 — always a REAL
// Vault, never a mock).
func vaultBackedConfig(env, addr, token string) *config.Config {
	return &config.Config{
		Env:            env,
		MerchantSource: config.MerchantSourceAPI,
		SecretBackend:  config.SecretBackendVault,
		Vault:          &config.VaultConfig{Enabled: true, Address: addr, AuthMethod: "token", Token: token},
	}
}

// #667 (a): production posture + DB store + no ENCRYPTION_MASTER_KEY refuses boot.
func TestBuild_ProdDBStoreNoMasterKey_RefusesBoot(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	cfg := &config.Config{Env: "production", MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB}
	_, err := Build(ctx, cfg, pool)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ENCRYPTION_MASTER_KEY")
	require.Contains(t, err.Error(), "secret_backend=vault")
}

// #667 (b): production + Vault-backed store boots without a master key (secrets
// live in Vault; the DB gate never applies). Runs against the real container.
func TestBuild_ProdVaultStoreNoMasterKey_Boots(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	addr, token := vaulttest.Addr(t)
	store, err := Build(ctx, vaultBackedConfig("production", addr, token), pool)
	require.NoError(t, err)
	require.NotNil(t, store.Secrets)
	require.True(t, store.Capabilities.KVRead, "root token must probe KV read")
	require.True(t, store.Capabilities.KVWrite, "root token must probe KV write")
	require.True(t, store.SecretWrite)
}

// #667 (c): dev + DB store + no key boots, with the loud plaintext warning.
func TestBuild_DevDBStoreNoMasterKey_BootsWithWarning(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	hook := logtest.NewGlobal()
	defer hook.Reset()
	cfg := &config.Config{Env: "dev", MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB}
	store, err := Build(ctx, cfg, pool)
	require.NoError(t, err)
	require.NotNil(t, store.Secrets)
	var warned bool
	for _, e := range hook.AllEntries() {
		if e.Level == log.WarnLevel && strings.Contains(e.Message, "PLAINTEXT") {
			warned = true
		}
	}
	require.True(t, warned, "dev boot without ENCRYPTION_MASTER_KEY must warn that secrets persist plaintext")
}

// #667 (d): production + DB store + master key boots and round-trips an encrypted
// secret (the raw DB row holds ciphertext, never the plaintext).
func TestBuild_ProdDBStoreWithKey_RoundTripsEncrypted(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	cfg := &config.Config{
		Env:            "production",
		MerchantSource: config.MerchantSourceAPI,
		SecretBackend:  config.SecretBackendDB,
		Encryption:     &config.EncryptionConfig{MasterKey: testMasterKey(t)},
	}
	store, err := Build(ctx, cfg, pool)
	require.NoError(t, err)

	mid, _ := registerMerchant(t, ctx, pool, "dbrt") // merchant_deks FK needs a real merchant
	const plaintext = "sk_live_667_roundtrip"
	_, err = store.Secrets.Put(ctx, mid, merchants.SecretStripeSecretKey, plaintext)
	require.NoError(t, err)

	got, err := store.Secrets.Get(ctx, mid, merchants.SecretStripeSecretKey)
	require.NoError(t, err)
	require.Equal(t, plaintext, got.Value)

	var raw string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT value FROM openrails.merchant_secrets WHERE merchant_id=$1::uuid AND name=$2`,
		mid.String(), merchants.SecretStripeSecretKey).Scan(&raw))
	require.NotEqual(t, plaintext, raw, "DB row must hold ciphertext, not plaintext")
	require.NotContains(t, raw, plaintext)
}
