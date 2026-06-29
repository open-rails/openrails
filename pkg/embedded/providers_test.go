package embedded

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

func TestApplyPaymentProvidersGeneratesLocalSelectors(t *testing.T) {
	rails, err := ApplyPaymentProviders([]PaymentProvider{
		{Config: config.RailConfig{Type: config.RailTypeStripe, SecretKey: "sk_live_a"}},
		{Config: config.RailConfig{Type: config.RailTypeStripe, Routing: config.RailRoutingLegacy, Stripe: &config.StripeRailConfig{SecretKey: "sk_live_b"}}},
		{Config: config.RailConfig{Type: config.RailTypeNMI, NMI: &config.NMIRailConfig{SecurityKey: "sec"}}},
	})
	require.NoError(t, err)
	require.NoError(t, config.ValidateRailSet(&config.Config{}, rails))

	require.Equal(t, "sk_live_a", rails["stripe"].SecretKey)
	require.Equal(t, "default", rails["stripe"].EffectiveRouting())
	require.Equal(t, "sk_live_b", rails["stripe_2"].SecretKey)
	require.Equal(t, "legacy", rails["stripe_2"].EffectiveRouting())
	require.Equal(t, "sec", rails["nmi"].SecurityKey)
}

func TestApplyPaymentProvidersAcceptsExplicitLocalSelectors(t *testing.T) {
	rails, err := ApplyPaymentProviders([]PaymentProvider{
		{
			Name:   "stripe_primary",
			Config: config.RailConfig{Type: config.RailTypeStripe, SecretKey: "sk_live_a"},
		},
		{
			Name:   "stripe_legacy",
			Config: config.RailConfig{Type: config.RailTypeStripe, Routing: config.RailRoutingLegacy, SecretKey: "sk_live_b"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "sk_live_a", rails["stripe_primary"].SecretKey)
	require.Equal(t, "sk_live_b", rails["stripe_legacy"].SecretKey)
}

func TestApplyPaymentProvidersRequiresTypeWhenNameDoesNotImplyIt(t *testing.T) {
	_, err := ApplyPaymentProviders([]PaymentProvider{{Name: "stripe_primary"}})
	require.ErrorContains(t, err, "requires config type")
}
