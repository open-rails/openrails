package tokens

import (
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/config"
)

// NormalizeForNetwork applies the Solana token pricing policy to a resolved
// token set. It never fails; tokens that cannot work are dropped with a loud
// warning:
//
//   - devnet: no pricing requirements (devnet money is fake).
//   - mainnet with a Pyth feed: feed pricing (for stablecoins, the depeg
//     failsafe).
//   - mainnet, known USD-pegged mint without a feed: $1.00 parity, warned.
//   - mainnet, non-USD peg (EURC) or unknown token without a feed: that token
//     is disabled; everything else keeps working.
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
		// Decimals are not configuration: they are read from the mint on-chain
		// at conversion time, where a bad precision fails the charge closed.
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
