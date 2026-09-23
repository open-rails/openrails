package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMerchantConfigSourceHardCut(t *testing.T) {
	for _, key := range []string{"MERCHANT_SOURCE", "MERCHANT_CONFIG_SOURCE"} {
		for _, value := range []string{"", "manifest", "api"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				t.Chdir(t.TempDir())
				t.Setenv(key, value)
				_, err := Load("")
				require.ErrorContains(t, err, "select secret_backend and merchant_config_http independently")
				_, err = LoadDatabase("")
				require.ErrorContains(t, err, "select secret_backend and merchant_config_http independently")
			})
		}
	}
	for _, key := range []string{"merchant_source", "merchant_config_source"} {
		t.Run(key, func(t *testing.T) {
			_, err := Load(configInputFile(t, key+": manifest\n"))
			require.ErrorContains(t, err, "select secret_backend and merchant_config_http independently")
		})
	}
	require.Empty(t, envKeyToConfigKey("MERCHANT_SOURCE"))
	require.Empty(t, envKeyToConfigKey("MERCHANT_CONFIG_SOURCE"))
}

func TestMerchantConfigHTTPIndependentOfBackend(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, backend := range []string{"snapshot", "vault"} {
		t.Setenv("SECRET_BACKEND", backend)
		for _, publish := range []string{"false", "true"} {
			t.Setenv("MERCHANT_CONFIG_HTTP", publish)
			cfg, err := Load("")
			require.NoError(t, err)
			require.Equal(t, backend, cfg.SecretStoreBackend())
			require.Equal(t, publish == "true", cfg.MerchantConfigHTTP)
		}
	}
}
