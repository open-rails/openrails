package config

import (
	"encoding/json"
	"testing"

	"github.com/goccy/go-yaml"
	"github.com/stretchr/testify/require"
)

func TestCardVaultConfigLoading(t *testing.T) {
	want := &CardVaultConfig{Kind: "basis_theory", TenantID: "tenant", KVMount: "cards"}
	var fromYAML, fromJSON Config
	require.NoError(t, yaml.Unmarshal([]byte("card_vault:\n  kind: basis_theory\n  tenant_id: tenant\n  kv_mount: cards\n"), &fromYAML))
	require.NoError(t, json.Unmarshal([]byte(`{"card_vault":{"kind":"basis_theory","tenant_id":"tenant","kv_mount":"cards"}}`), &fromJSON))
	require.Equal(t, want, fromYAML.CardVault)
	require.Equal(t, want, fromJSON.CardVault)
}

func TestCardVaultMount(t *testing.T) {
	for _, tc := range []struct {
		name, merchantMount, platformMount, want string
		invalid                                  bool
	}{
		{name: "files", platformMount: "cards", want: "cards"},
		{name: "existing shared mount", merchantMount: "merchant", want: "merchant"},
		{name: "separate mounts", merchantMount: "merchant", platformMount: "cards", want: "cards"},
		{name: "normalized mount", platformMount: " /billing/cards/ ", want: "billing/cards"},
		{name: "no mount", invalid: true},
		{name: "invalid explicit mount", merchantMount: "merchant", platformMount: "../cards", invalid: true},
		{name: "empty explicit path", merchantMount: "merchant", platformMount: "/", invalid: true},
		{name: "space in path", merchantMount: "merchant", platformMount: "card secrets", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := &Config{Vault: &VaultConfig{KVMount: tc.merchantMount}, CardVault: &CardVaultConfig{
				Kind: "basis_theory", TenantID: "tenant", KVMount: tc.platformMount,
			}}
			if tc.invalid {
				require.Error(t, validateCardVault(cfg))
				Degrade(cfg)
				require.Nil(t, cfg.CardVault)
			} else {
				require.NoError(t, validateCardVault(cfg))
				mount, err := CardVaultKVMount(cfg)
				require.NoError(t, err)
				require.Equal(t, tc.want, mount)
			}
			require.Equal(t, tc.merchantMount, MerchantConfigKVMount(cfg))
			err := RefuseDeclarationBesideVault(cfg, "shop", MerchantDeclaration{DisplayName: "Shop"})
			if tc.merchantMount == "" {
				require.NoError(t, err, "platform credentials do not select the merchant configuration source")
			} else {
				require.ErrorIs(t, err, ErrDeclarationBesideVault)
			}
		})
	}
}
