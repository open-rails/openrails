package checkout

import (
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTokenPricesAreNeverOfferedOnCardRails(t *testing.T) {
	svc := &CheckoutAttemptService{solanaTransactionService: noopSolanaTransactions{}}
	for _, currency := range []string{"SOL", "USDC"} {
		price := &models.Price{Currency: currency, Amount: 1_000_000, PSPLinks: map[string]map[string]string{"solana": {models.RailKeyRail: "solana"}}}
		for _, rail := range []string{"nmi", "stripe", "ccbill"} {
			require.Equal(t, models.CheckoutRoutingSkipCurrencyUnsupported, svc.checkoutRailSkipReason(price, railTarget{PSP: rail, Rail: rail}, &config.ResolvedPSP{}, models.CheckoutAttemptModeOneOff))
		}
		provider := &config.ResolvedPSP{Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{currency: {Mint: "configured"}}}}
		require.Empty(t, svc.checkoutRailSkipReason(price, railTarget{PSP: "solana", Rail: "solana"}, provider, models.CheckoutAttemptModeOneOff))
		price.BillingIntervalHours = new(720)
		require.Equal(t, models.CheckoutRoutingSkipCurrencyUnsupported, svc.checkoutRailSkipReason(price, railTarget{PSP: "solana", Rail: "solana"}, provider, models.CheckoutAttemptModeSubscription), "new recurring token prices must not silently publish USD plans")
	}
}
