package tokens

import (
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
)

// NormalizeForNetwork applies the #360 Solana token pricing policy to a
// resolved token set. It NEVER fails — tokens that cannot function are
// dropped with a loud warning:
//
//   - devnet: NO pricing requirements at all — devnet money is fake.
//   - mainnet, Pyth feed exists for the symbol: feed pricing (for stablecoins
//     the feed is the depeg failsafe).
//   - mainnet, known USD-pegged stablecoin mint without a feed: kept, priced
//     at $1.00 parity, LOUD warning (no depeg protection).
//   - mainnet, non-USD-pegged stablecoin (e.g. EURC) or unknown token without
//     a feed: that TOKEN is disabled with a loud warning; everything else
//     keeps working.
//
// Formerly the boot-time configureSolanaRail; #788 moved it to resolution
// time since the token set is per-merchant armed state, not boot config.
func NormalizeForNetwork(network string, tokens map[string]config.TokenConfig) map[string]config.TokenConfig {
	normalized := make(map[string]config.TokenConfig, len(tokens))
	for symbol, token := range tokens {
		normalizedSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		if normalizedSymbol == "" {
			log.Warn("⚠️  solana token with empty symbol in configuration; entry dropped")
			continue
		}
		if strings.TrimSpace(token.Mint) == "" {
			log.Warnf("⚠️  solana token %s has no mint configured; payments in %s unavailable", normalizedSymbol, normalizedSymbol)
			continue
		}
		if token.Decimals < 0 {
			log.Warnf("⚠️  solana token %s has invalid decimals (%d); payments in %s unavailable", normalizedSymbol, token.Decimals, normalizedSymbol)
			continue
		}
		if strings.TrimSpace(token.Name) == "" {
			token.Name = normalizedSymbol
		}

		// Pricing policy applies to MAINNET only: devnet money is fake, so a
		// devnet deployment never needs price feeds (or Hermes) at all.
		if network != "devnet" {
			switch decision, sc := ClassifyPricing(normalizedSymbol, token.Mint); decision {
			case TokenPricingFeed:
				// Live Pyth pricing; for stablecoins the feed doubles as the
				// depeg failsafe.
			case TokenPricingUSDParity:
				log.Warnf("⚠️  solana token %s has no pyth price feed; degrading to $1.00 USD parity (known USD-pegged stablecoin, NO depeg protection)", normalizedSymbol)
			case TokenPricingDisabled:
				if sc.Symbol != "" {
					log.Warnf("⚠️  solana token %s is pegged to %s and has no price feed; payments in %s unavailable (cannot default a non-USD peg to USD parity)", normalizedSymbol, strings.ToUpper(sc.Peg), normalizedSymbol)
				} else {
					log.Warnf("⚠️  solana token %s has no pyth price feed and is not a known stablecoin; payments in %s unavailable", normalizedSymbol, normalizedSymbol)
				}
				continue
			}
		}
		normalized[normalizedSymbol] = token
	}
	return normalized
}
