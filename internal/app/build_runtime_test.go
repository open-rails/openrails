package app

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestRiverSchemaDefaultsAndOverride(t *testing.T) {
	rt := &Runtime{}
	require.Equal(t, "public", rt.riverSchemaOrDefault())
	rt.SetRiverSchema("jobs")
	require.Equal(t, "jobs", rt.riverSchemaOrDefault())
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
