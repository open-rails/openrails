package app

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// TestStandaloneRiverSchemaIsAlwaysPublic verifies the #545 rule: when OpenRails
// constructs its own River client, River tables ALWAYS live in `public`
// (config.RiverSchema, River's documented default) — decoupled from the
// OpenRails billing schema (reversing the #165 coupling) so the billing schema
// stays a clean whole-schema dump (#544). db.schema no longer changes it.
func TestStandaloneRiverSchemaIsAlwaysPublic(t *testing.T) {
	t.Parallel()

	require.Equal(t, "public", config.RiverSchema)

	t.Run("default", func(t *testing.T) {
		require.Equal(t, config.RiverSchema, standaloneRiverSchema(config.GetDefaultBillingConfig()))
	})

	t.Run("custom db.schema does not change it", func(t *testing.T) {
		cfg := config.GetDefaultBillingConfig()
		cfg.DB.Schema = "host_billing"
		require.Equal(t, config.RiverSchema, standaloneRiverSchema(cfg))
	})

	t.Run("nil db/config", func(t *testing.T) {
		require.Equal(t, config.RiverSchema, standaloneRiverSchema(&config.Config{}))
		require.Equal(t, config.RiverSchema, standaloneRiverSchema(nil))
	})
}

func pythCfg(t *testing.T, testMode bool) *config.Config {
	t.Helper()
	cfg := config.GetDefaultBillingConfig()
	cfg.ProviderWriteMode = config.ProviderWriteModeFull
	posture := config.CredentialPostureLive
	if testMode {
		posture = config.CredentialPostureSandbox
	}
	cfg.TestMode = posture
	return cfg
}

func TestCreatePythPriceProviderDevnetParity(t *testing.T) {
	t.Parallel()

	t.Run("devnet provider never errors for feedless tokens (no Hermes required)", func(t *testing.T) {
		provider, err := createPythPriceProvider(pythCfg(t, true))
		require.NoError(t, err)
		require.NotNil(t, provider)
		// Unknown symbol -> the inner pyth client fails locally on the missing
		// feed (no network call) and the devnet wrapper degrades to parity.
		price, err := provider.PriceUSD(context.Background(), "WEIRD")
		require.NoError(t, err)
		require.Equal(t, 1.0, price)
	})

	t.Run("mainnet provider still errors on unknown feed", func(t *testing.T) {
		provider, err := createPythPriceProvider(pythCfg(t, false))
		require.NoError(t, err)
		require.NotNil(t, provider)
		_, err = provider.PriceUSD(context.Background(), "WEIRD")
		require.Error(t, err)
	})
}
