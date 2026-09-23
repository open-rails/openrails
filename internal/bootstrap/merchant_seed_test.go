package bootstrap

import (
	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestResolvePushMerchantConfigOptions(t *testing.T) {
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB, config.SecretBackendVault} {
		cfg := &config.Config{SecretBackend: backend}
		for _, seed := range []bool{false, true} {
			opts, err := ResolvePushMerchantConfigOptions(cfg, seed, false, false, false)
			require.NoError(t, err)
			require.Equal(t, seed, opts.Insert)
			_, err = ResolvePushMerchantConfigOptions(cfg, seed, true, true, false)
			require.ErrorContains(t, err, "expected revision")
			_, err = ResolvePushMerchantConfigOptions(cfg, seed, true, false, true)
			require.ErrorContains(t, err, "expected revision")
		}
	}
}
