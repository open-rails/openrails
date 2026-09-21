package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndependentCatalogAuthority(t *testing.T) {
	for _, merchantSource := range []string{MerchantSourceManifest, MerchantSourceAPI} {
		for _, catalogSource := range []string{"", CatalogSourceManifest, CatalogSourceAPI} {
			t.Run(merchantSource+"/"+catalogSource, func(t *testing.T) {
				cfg := validationConfig("production")
				cfg.MerchantSource, cfg.CatalogSource = merchantSource, catalogSource
				if merchantSource == MerchantSourceAPI {
					cfg.SecretBackend = SecretBackendVault
					cfg.Vault = &VaultConfig{Enabled: true}
				}
				require.NoError(t, Validate(cfg))
				want := catalogSource
				if want == "" {
					want = merchantSource
				}
				require.Equal(t, want, cfg.CatalogSourceMode())
				require.Equal(t, want == CatalogSourceManifest, cfg.IsManifestCatalogSource())
				require.Equal(t, merchantSource == MerchantSourceManifest, cfg.IsManifestMerchantSource())
			})
		}
	}
	var nilConfig *Config
	require.Equal(t, CatalogSourceManifest, nilConfig.CatalogSourceMode())
	cfg := validationConfig("development")
	cfg.CatalogSource = "ap1"
	require.ErrorContains(t, Validate(cfg), "catalog_source")
}

func TestCatalogSourceLoadsIndependently(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENV", "development")
	t.Setenv("MERCHANT_SOURCE", "manifest")
	t.Setenv("CATALOG_SOURCE", "api")
	cfg, err := Load("")
	require.NoError(t, err)
	require.True(t, cfg.IsManifestMerchantSource())
	require.Equal(t, CatalogSourceAPI, cfg.CatalogSourceMode())
	t.Setenv("CATALOG_SOURCE", "typo")
	_, err = Load("")
	require.ErrorContains(t, err, "catalog_source")
}
