package bootstrap

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// #851: push-merchant-config flag doctrine per merchant-source mode.
func TestResolvePushMerchantConfigOptions(t *testing.T) {
	manifestCfg := &config.Config{MerchantSource: config.MerchantSourceManifest}
	apiCfg := &config.Config{MerchantSource: config.MerchantSourceAPI}

	t.Run("manifest mode passes mutation flags through", func(t *testing.T) {
		opts, err := ResolvePushMerchantConfigOptions(manifestCfg, false, true, true, true)
		require.NoError(t, err)
		require.Equal(t, MerchantManifestReconcileOptions{Insert: true, Overwrite: true, Prune: true}, opts)
	})

	t.Run("manifest mode refuses --seed", func(t *testing.T) {
		_, err := ResolvePushMerchantConfigOptions(manifestCfg, true, false, false, false)
		require.ErrorContains(t, err, "--seed is the merchant_source=api importer gate")
	})

	t.Run("api mode refuses without --seed", func(t *testing.T) {
		_, err := ResolvePushMerchantConfigOptions(apiCfg, false, false, false, false)
		require.ErrorContains(t, err, "seed-once importer")
		_, err = ResolvePushMerchantConfigOptions(apiCfg, false, true, false, false)
		require.ErrorContains(t, err, "seed-once importer")
	})

	t.Run("api mode --seed refuses mutation flags", func(t *testing.T) {
		for _, flags := range [][3]bool{{true, false, false}, {false, true, false}, {false, false, true}} {
			_, err := ResolvePushMerchantConfigOptions(apiCfg, true, flags[0], flags[1], flags[2])
			require.ErrorContains(t, err, "does not combine")
		}
	})

	t.Run("api mode --seed is create-only", func(t *testing.T) {
		opts, err := ResolvePushMerchantConfigOptions(apiCfg, true, false, false, false)
		require.NoError(t, err)
		require.Equal(t, MerchantManifestReconcileOptions{Insert: true}, opts)
	})
}
