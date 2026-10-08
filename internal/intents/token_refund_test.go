package intents

import (
	"testing"

	"github.com/open-rails/openrails/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCardRefundsRefuseTokenAmountsBeforeProviderIO(t *testing.T) {
	for _, currency := range []string{"SOL", "USDC"} {
		intent := refundIntent(t, TypeStripeRefund, func(p *RefundPayload) { p.Currency = currency; p.ProviderTarget = "ch_original" })
		provider := &fakeStripeRefunds{}
		stripe := NewStripeRefundHandler(nil, &config.Config{}, nil, nil, nil)
		stripe.Stripe = provider
		out := stripe.Execute(t.Context(), intent)
		require.Equal(t, OutcomeTerminal, out.Class)
		require.Contains(t, out.Reason, "not supported by card payment rails")
		require.Empty(t, provider.gotParams.ChargeID, "Stripe must never receive native token units in its currencyless refund amount")
		intent.IntentType, intent.Rail = TypeNMIRefund, "nmi"
		out = (&NMIRefundHandler{}).Execute(t.Context(), intent)
		require.Equal(t, OutcomeTerminal, out.Class)
		require.Contains(t, out.Reason, "not supported by card payment rails")
	}
}
