package intents

import (
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

func TestCardRefundsRefuseTokenAmountsBeforeProviderIO(t *testing.T) {
	for _, currency := range []string{"SOL", "USDC"} {
		intent := refundIntent(t, TypeStripeRefund, func(p *RefundPayload) { p.Currency = currency; p.ProviderTarget = "ch_original" })
		provider := stripemock.NewUnstarted(stripemock.Options{})
		rails := railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_123"}}}
		stripe := NewStripeRefundHandler(nil, &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, rails, nil, stripeapi.NewFactory(provider))
		out := stripe.Execute(t.Context(), intent)
		require.Equal(t, OutcomeTerminal, out.Class)
		require.Contains(t, out.Reason, "not supported by card payment rails")
		require.Empty(t, provider.Submitted(""), "Stripe must never receive native token units in its currencyless refund amount")
		intent.IntentType, intent.Rail = TypeNMIRefund, "nmi"
		out = (&NMIRefundHandler{}).Execute(t.Context(), intent)
		require.Equal(t, OutcomeTerminal, out.Class)
		require.Contains(t, out.Reason, "not supported by card payment rails")
	}
}
