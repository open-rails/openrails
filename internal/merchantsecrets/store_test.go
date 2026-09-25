package merchantsecrets

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/vault"
)

// FC-5: a declared backend is served as declared or refused; never a silent fallback.
func TestSecretBackendIsDeclaredNeverFallback(t *testing.T) {
	full := vault.Capabilities{KVRead: true, KVWrite: true}
	for _, tc := range []struct {
		name      string
		backend   string
		connected bool
		caps      vault.Capabilities
		useVault  bool
		errPart   string
	}{
		{"db ignores a full vault", config.SecretBackendDB, true, full, false, ""},
		{"db without vault", config.SecretBackendDB, false, vault.Capabilities{}, false, ""},
		{"vault full KV", config.SecretBackendVault, true, full, true, ""},
		{"vault read-only KV still serves", config.SecretBackendVault, true, vault.Capabilities{KVRead: true}, true, ""},
		{"vault transit-only refuses", config.SecretBackendVault, true, vault.Capabilities{}, false, `cannot read the KV mount "secret"`},
		{"vault not connected refuses", config.SecretBackendVault, false, full, false, "requires vault.enabled"},
		{"unknown backend refuses", "s3", true, full, false, "unknown credential backend"},
		{"snapshot is not a managed backend", "snapshot", true, full, false, "unknown credential backend"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useVault, err := gateSecretBackend(tc.backend, tc.connected, tc.caps, DefaultVaultKVMount)
			if tc.errPart != "" {
				require.ErrorContains(t, err, tc.errPart)
				require.False(t, useVault)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.useVault, useVault)
		})
	}
}

// FC-3: DB custody without a master key refuses boot in every posture.
func TestDBCustodyRequiresMasterKey(t *testing.T) {
	require.NoError(t, enforceEncryptionPosture(true, true))
	require.NoError(t, enforceEncryptionPosture(true, false))
	for _, required := range []bool{true, false} {
		err := enforceEncryptionPosture(false, required)
		require.ErrorContains(t, err, "ENCRYPTION_MASTER_KEY")
		require.ErrorContains(t, err, "secret_backend=vault")
	}
}

func TestDeriveRouteGates(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		useVault, connected, encryption bool
		caps                            vault.Capabilities
		canSign, secretWrite            bool
	}{
		{"db + encryption signs locally", false, false, true, vault.Capabilities{}, true, true},
		{"db without encryption cannot sign", false, false, false, vault.Capabilities{}, false, true},
		{"db + transit-only vault signs via connection", false, true, false, vault.Capabilities{}, true, true},
		{"vault KV rw", true, true, false, vault.Capabilities{KVRead: true, KVWrite: true}, true, true},
		{"vault KV read-only disables writes", true, true, false, vault.Capabilities{KVRead: true}, true, false},
		{"vault KV no read and no connection cannot sign", true, false, true, vault.Capabilities{}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canSign, secretWrite := deriveRouteGates(tc.useVault, tc.connected, tc.encryption, tc.caps)
			require.Equal(t, tc.canSign, canSign)
			require.Equal(t, tc.secretWrite, secretWrite)
		})
	}
}

func TestBuildGuardsAndDefaults(t *testing.T) {
	ctx := context.Background()
	_, err := Build(ctx, nil, nil)
	require.ErrorContains(t, err, "config and db pool are required")
	_, err = Build(ctx, &config.Config{}, nil)
	require.Error(t, err)
	_, err = BuildManifest(ctx, nil, nil, nil)
	require.ErrorContains(t, err, "credential config is required")

	for _, cfg := range []*config.Config{nil, {}, {Vault: &config.VaultConfig{Enabled: false, Token: "t"}}} {
		s, err := BuildTransit(ctx, cfg)
		require.NoError(t, err)
		require.Nil(t, s.SolanaTransit, "disabled vault opens no transit client")
		require.NoError(t, s.Probe(ctx), "a store without a vault client has no separate liveness")
		require.NoError(t, s.State())
		s.Close()
	}
	var nilStore *Store
	require.NoError(t, nilStore.Probe(ctx))
	nilStore.Close()

	require.Equal(t, "secret", resolveVaultKVMount(nil))
	require.Equal(t, "transit", resolveVaultTransitMount(&config.VaultConfig{}))
	require.Equal(t, "kv2", resolveVaultKVMount(&config.VaultConfig{KVMount: "kv2"}))
	require.Equal(t, "sign", resolveVaultTransitMount(&config.VaultConfig{TransitMount: "sign"}))
}
