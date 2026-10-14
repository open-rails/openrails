package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlatformVaultMount(t *testing.T) {
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
			cfg := &Config{Vault: &VaultConfig{KVMount: tc.merchantMount}, PlatformVault: &PlatformVaultConfig{
				Kind: "basis_theory", TenantID: "tenant", KVMount: tc.platformMount,
			}}
			if tc.invalid {
				require.Error(t, validatePlatformVault(cfg))
				Degrade(cfg)
				require.Nil(t, cfg.PlatformVault)
			} else {
				require.NoError(t, validatePlatformVault(cfg))
				mount, err := PlatformVaultKVMount(cfg)
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
