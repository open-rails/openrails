package embedded

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

func TestApplyPaymentProvidersGeneratesLocalSelectors(t *testing.T) {
	cfg := &config.Config{}
	err := ApplyPaymentProviders(cfg, []PaymentProvider{
		{Config: config.ProcessorConfig{Type: config.ProcessorTypeStripe, SecretKey: "sk_live_a"}},
		{Config: config.ProcessorConfig{Type: config.ProcessorTypeStripe, Role: config.ProcessorRoleLegacy, SecretKey: "sk_live_b"}},
		{Config: config.ProcessorConfig{Type: config.ProcessorTypeNMI, SecurityKey: "sec"}},
	})
	require.NoError(t, err)

	require.Equal(t, "sk_live_a", cfg.Processors["stripe"].SecretKey)
	require.Equal(t, "primary", cfg.Processors["stripe"].EffectiveRole())
	require.Equal(t, "sk_live_b", cfg.Processors["stripe_2"].SecretKey)
	require.Equal(t, "legacy", cfg.Processors["stripe_2"].EffectiveRole())
	require.Equal(t, "sec", cfg.Processors["nmi"].SecurityKey)
}

func TestApplyPaymentProvidersAcceptsExplicitLocalSelectors(t *testing.T) {
	cfg := &config.Config{}
	err := ApplyPaymentProviders(cfg, []PaymentProvider{
		{
			Name:   "stripe_primary",
			Config: config.ProcessorConfig{Type: config.ProcessorTypeStripe, SecretKey: "sk_live_a"},
		},
		{
			Name:   "stripe_legacy",
			Config: config.ProcessorConfig{Type: config.ProcessorTypeStripe, Role: config.ProcessorRoleLegacy, SecretKey: "sk_live_b"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "sk_live_a", cfg.Processors["stripe_primary"].SecretKey)
	require.Equal(t, "sk_live_b", cfg.Processors["stripe_legacy"].SecretKey)
}

func TestApplyPaymentProvidersRequiresTypeWhenNameDoesNotImplyIt(t *testing.T) {
	err := ApplyPaymentProviders(&config.Config{}, []PaymentProvider{{Name: "stripe_primary"}})
	require.ErrorContains(t, err, "requires config type")
}
