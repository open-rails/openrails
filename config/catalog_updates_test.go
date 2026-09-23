package config

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCatalogUpdatesIndependentOfCredentialSource(t *testing.T) {
	require.False(t, GetDefaultBillingConfig().AllowCatalogUpdates)
	for _, source := range []string{MerchantConfigSourceManifest, MerchantConfigSourceAPI} {
		for _, allow := range []bool{false, true} {
			t.Run(source+"/"+strconv.FormatBool(allow), func(t *testing.T) {
				cfg := validationConfig("production")
				cfg.MerchantConfigSource, cfg.AllowCatalogUpdates = source, allow
				if source == MerchantConfigSourceAPI {
					cfg.SecretBackend = SecretBackendVault
					cfg.Vault = &VaultConfig{Enabled: true}
				}
				require.NoError(t, Validate(cfg))
				require.Equal(t, allow, cfg.AllowCatalogUpdates)
				require.Equal(t, source == MerchantConfigSourceManifest, cfg.IsManifestMerchantConfigSource())
			})
		}
	}
}
