package config

import (
	billing "github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogUpdatesConfigLoading(t *testing.T) {
	t.Chdir(t.TempDir())

	t.Setenv("SECRET_BACKEND", "snapshot")
	cfg, err := Load("")
	require.NoError(t, err)
	require.False(t, cfg.AllowCatalogUpdates)
	t.Setenv("ALLOW_CATALOG_UPDATES", "true")
	cfg, err = Load("")
	require.NoError(t, err)
	require.True(t, cfg.AllowCatalogUpdates)
	require.Equal(t, billing.SecretBackendSnapshot, cfg.SecretStoreBackend())
	t.Setenv("ALLOW_CATALOG_UPDATES", "invalid")
	_, err = Load("")
	require.Error(t, err)
}

func TestRetiredCatalogSourceRefusesLoad(t *testing.T) {
	t.Chdir(t.TempDir())

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
	require.NoError(t, os.WriteFile(path, []byte("catalog_source: api\n"), 0600))
	_, err := Load(path)
	require.ErrorContains(t, err, "allow_catalog_updates")
	require.Empty(t, envKeyToConfigKey("CATALOG_SOURCE"))
	require.Equal(t, "allow_catalog_updates", envKeyToConfigKey("ALLOW_CATALOG_UPDATES"))
}
