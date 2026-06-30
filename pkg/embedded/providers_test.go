package embedded

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestApplyPaymentProvidersGeneratesLocalSelectors(t *testing.T) {
	rails, err := ApplyPaymentProviders([]PaymentProvider{
		{Config: config.ProviderAccountConfig{Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_live_a"}}},
		{Config: config.ProviderAccountConfig{Rail: models.RailStripe, Routing: config.RailRoutingLegacy, Stripe: &config.StripeRailConfig{SecretKey: "sk_live_b"}}},
		{Config: config.ProviderAccountConfig{Rail: models.RailNMI, NMI: &config.NMIRailConfig{SecurityKey: "sec"}}},
	})
	require.NoError(t, err)
	require.NoError(t, config.ValidateRailSet(&config.Config{}, rails))

	require.Equal(t, "sk_live_a", rails["stripe"].Stripe.SecretKey)
	require.Equal(t, "default", rails["stripe"].EffectiveRouting())
	require.Equal(t, "sk_live_b", rails["stripe_2"].Stripe.SecretKey)
	require.Equal(t, "legacy", rails["stripe_2"].EffectiveRouting())
	require.Equal(t, "sec", rails["nmi"].NMI.SecurityKey)
}

func TestApplyPaymentProvidersAcceptsExplicitLocalSelectors(t *testing.T) {
	rails, err := ApplyPaymentProviders([]PaymentProvider{
		{
			Name:   "stripe_primary",
			Config: config.ProviderAccountConfig{Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_live_a"}},
		},
		{
			Name:   "stripe_legacy",
			Config: config.ProviderAccountConfig{Rail: models.RailStripe, Routing: config.RailRoutingLegacy, Stripe: &config.StripeRailConfig{SecretKey: "sk_live_b"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "sk_live_a", rails["stripe_primary"].Stripe.SecretKey)
	require.Equal(t, "sk_live_b", rails["stripe_legacy"].Stripe.SecretKey)
}

func TestApplyPaymentProvidersRequiresTypeWhenNameDoesNotImplyIt(t *testing.T) {
	_, err := ApplyPaymentProviders([]PaymentProvider{{Name: "stripe_primary"}})
	require.ErrorContains(t, err, "requires config rail")
}
