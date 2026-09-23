package config

import (
	billing "github.com/open-rails/openrails/config"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMerchantConfigSourceHardCut(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENV", "development")
	t.Run("new environment spelling", func(t *testing.T) {
		t.Setenv("MERCHANT_CONFIG_SOURCE", "api")
		t.Setenv("SECRET_BACKEND", "db")
		cfg, err := Load("")
		require.NoError(t, err)
		require.Equal(t, billing.MerchantConfigSourceAPI, cfg.MerchantConfigSourceMode())
	})
	t.Run("new config spelling", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.yaml")
		require.NoError(t, os.WriteFile(path, []byte("env: development\nmerchant_config_source: manifest\nallow_catalog_updates: true\n"), 0600))
		cfg, err := Load(path)
		require.NoError(t, err)
		require.True(t, cfg.IsManifestMerchantConfigSource())
		require.True(t, cfg.AllowCatalogUpdates)
	})
	for _, value := range []string{"", "manifest", "api"} {
		t.Run("retired env="+value, func(t *testing.T) {
			t.Setenv("MERCHANT_SOURCE", value)
			t.Setenv("MERCHANT_CONFIG_SOURCE", "manifest")
			_, err := Load("")
			require.ErrorContains(t, err, "use merchant_config_source / MERCHANT_CONFIG_SOURCE")
			_, err = LoadDatabase("")
			require.ErrorContains(t, err, "use merchant_config_source / MERCHANT_CONFIG_SOURCE")
		})
	}
	for _, suffix := range []string{"", "merchant_config_source: manifest\n"} {
		t.Run("retired config "+suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte("env: development\nmerchant_source: manifest\n"+suffix), 0600))
			_, err := Load(path)
			require.ErrorContains(t, err, "use merchant_config_source / MERCHANT_CONFIG_SOURCE")
		})
	}
	require.Empty(t, envKeyToConfigKey("MERCHANT_SOURCE"), "old env is rejected explicitly, never consumed as an alias")
	require.Equal(t, "merchant_config_source", envKeyToConfigKey("MERCHANT_CONFIG_SOURCE"))
}
