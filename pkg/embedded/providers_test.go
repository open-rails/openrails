package embedded

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

func TestApplyPaymentProvidersGeneratesLocalSelectors(t *testing.T) {
	processors, err := ApplyPaymentProviders([]PaymentProvider{
		{Config: config.ProcessorConfig{Type: config.ProcessorTypeStripe, SecretKey: "sk_live_a"}},
		{Config: config.ProcessorConfig{Type: config.ProcessorTypeStripe, Role: config.ProcessorRoleLegacy, SecretKey: "sk_live_b"}},
		{Config: config.ProcessorConfig{Type: config.ProcessorTypeNMI, SecurityKey: "sec"}},
	})
	require.NoError(t, err)

	require.Equal(t, "sk_live_a", processors["stripe"].SecretKey)
	require.Equal(t, "primary", processors["stripe"].EffectiveRole())
	require.Equal(t, "sk_live_b", processors["stripe_2"].SecretKey)
	require.Equal(t, "legacy", processors["stripe_2"].EffectiveRole())
	require.Equal(t, "sec", processors["nmi"].SecurityKey)
}

func TestApplyPaymentProvidersAcceptsExplicitLocalSelectors(t *testing.T) {
	processors, err := ApplyPaymentProviders([]PaymentProvider{
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
	require.Equal(t, "sk_live_a", processors["stripe_primary"].SecretKey)
	require.Equal(t, "sk_live_b", processors["stripe_legacy"].SecretKey)
}

func TestApplyPaymentProvidersRequiresTypeWhenNameDoesNotImplyIt(t *testing.T) {
	_, err := ApplyPaymentProviders([]PaymentProvider{{Name: "stripe_primary"}})
	require.ErrorContains(t, err, "requires config type")
}
