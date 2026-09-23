package config

import (
	"os"
	"path/filepath"
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

func TestCatalogUpdatesConfigLoading(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENV", "development")
	t.Setenv("MERCHANT_CONFIG_SOURCE", "manifest")
	cfg, err := Load("")
	require.NoError(t, err)
	require.False(t, cfg.AllowCatalogUpdates)
	t.Setenv("ALLOW_CATALOG_UPDATES", "true")
	cfg, err = Load("")
	require.NoError(t, err)
	require.True(t, cfg.AllowCatalogUpdates)
	require.True(t, cfg.IsManifestMerchantConfigSource())
	t.Setenv("ALLOW_CATALOG_UPDATES", "invalid")
	_, err = Load("")
	require.Error(t, err)
}

func TestRetiredCatalogSourceRefusesLoad(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENV", "development")
	for _, value := range []string{"", "manifest", "api"} {
		t.Run("env="+value, func(t *testing.T) {
			t.Setenv("CATALOG_SOURCE", value)
			_, err := Load("")
			require.ErrorContains(t, err, "catalogs always use the database")
			_, err = LoadDatabase("")
			require.ErrorContains(t, err, "catalogs always use the database")
		})
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("env: development\ncatalog_source: api\n"), 0600))
	_, err := Load(path)
	require.ErrorContains(t, err, "allow_catalog_updates")
	require.Empty(t, envKeyToConfigKey("CATALOG_SOURCE"))
	require.Equal(t, "allow_catalog_updates", envKeyToConfigKey("ALLOW_CATALOG_UPDATES"))
}
