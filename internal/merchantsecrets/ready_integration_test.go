//go:build integration

package merchantsecrets

import (
	"context"
	"testing"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
)

// unreachableVaultClient points at an address nothing listens on, so any
// logical call fails fast (connection refused) instead of hanging.
func unreachableVaultClient(t *testing.T) *vaultapi.Client {
	t.Helper()
	apiCfg := vaultapi.DefaultConfig()
	apiCfg.Address = "http://127.0.0.1:1"
	apiCfg.HttpClient.Timeout = 2 * time.Second
	client, err := vaultapi.NewClient(apiCfg)
	require.NoError(t, err)
	client.SetToken("irrelevant")
	return client
}

// TestStorePing proves the #748 live-reachability probe backing
// Runtime.Ready's merchant-secret check: a DB-backed store's Ping is always a
// no-op (arming and liveness collapse to the same DB ping), a reachable
// Vault-backed store's Ping succeeds, and — critically — a Vault that goes
// unreachable AFTER a successful Build (paused, network partition) fails
// Ping. Arming succeeding at boot must not be mistaken for staying healthy.
func TestStorePing(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)

	t.Run("DB-backed store: Ping is always a no-op", func(t *testing.T) {
		store, err := Build(ctx, &config.Config{Env: "dev", MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB}, pool)
		require.NoError(t, err)
		require.NoError(t, store.Ping(ctx))
	})

	t.Run("Vault-backed store: reachable Vault pings clean", func(t *testing.T) {
		addr, token := vaulttest.Addr(t)
		store, err := Build(ctx, vaultBackedConfig("production", addr, token), pool)
		require.NoError(t, err)
		require.NoError(t, store.Ping(ctx))
	})

	t.Run("Vault-backed store: unreachable Vault fails Ping naming the dependency", func(t *testing.T) {
		addr, token := vaulttest.Addr(t)
		store, err := Build(ctx, vaultBackedConfig("production", addr, token), pool)
		require.NoError(t, err, "Build succeeds against a healthy Vault")
		require.NoError(t, store.Ping(ctx), "sanity: healthy before we simulate an outage")

		// Simulate Vault going unreachable AFTER boot (paused / network
		// partition) on the SAME Store, without disturbing the shared
		// vaulttest container other tests reuse.
		store.vclient = unreachableVaultClient(t)

		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		err = store.Ping(pingCtx)
		require.Error(t, err, "an unreachable Vault must fail Ping, not just Build")
		require.Contains(t, err.Error(), "vault unreachable")
	})
}
