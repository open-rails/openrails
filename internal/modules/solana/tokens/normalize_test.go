package tokens

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// #360 Solana token pricing policy: resolution must NEVER fail over token
// pricing (degrade-not-die). Incident: the doujins stack crash-looped with
// "solana token USD1 missing pyth price feed". #788 moved the policy from the
// boot bridge (configureSolanaRail) to resolution time (NormalizeForNetwork).
// ----------------------------------------------------------------------------

func TestNormalizeForNetworkPolicyMatrix(t *testing.T) {
	t.Parallel()

	t.Run("devnet: unknown token without feed is kept (no pricing requirements)", func(t *testing.T) {
		out := NormalizeForNetwork("devnet", map[string]config.TokenConfig{
			"WEIRD": {Name: "Weird Devnet Coin", Mint: "WeirdMint1111111111111111111111111111111111"},
		})
		require.Contains(t, out, "WEIRD")
	})

	t.Run("mainnet: USD-pegged stablecoin without feed degrades to parity, stays enabled", func(t *testing.T) {
		// USDT is in the stablecoin registry (USD peg) but has no built-in feed.
		out := NormalizeForNetwork("mainnet", map[string]config.TokenConfig{
			"USDT": {Name: "Tether", Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"},
		})
		require.Contains(t, out, "USDT")
	})

	t.Run("mainnet: non-USD-pegged stablecoin without feed is disabled", func(t *testing.T) {
		out := NormalizeForNetwork("mainnet", map[string]config.TokenConfig{
			"EURC": {Name: "Euro Coin", Mint: "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"},
			"USDC": {Name: "USD Coin", Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		})
		require.NotContains(t, out, "EURC", "EURC cannot default to USD parity")
		require.Contains(t, out, "USDC", "other tokens keep working")
	})

	t.Run("mainnet: unknown token without feed is disabled", func(t *testing.T) {
		out := NormalizeForNetwork("mainnet", map[string]config.TokenConfig{
			"NOPE": {Name: "Mystery Coin", Mint: "NopeMint1111111111111111111111111111111111"},
			"SOL":  {Name: "Solana", Mint: "So11111111111111111111111111111111111111112"},
		})
		require.NotContains(t, out, "NOPE")
		require.Contains(t, out, "SOL")
	})

	t.Run("mainnet: the curated default set (incl. USD1/USDG) survives", func(t *testing.T) {
		out := NormalizeForNetwork("mainnet", ForNetwork("mainnet"))
		for _, sym := range []string{"SOL", "USDC", "PYUSD", "USD1", "USDG"} {
			require.Contains(t, out, sym)
		}
	})

	t.Run("devnet: the curated default set survives", func(t *testing.T) {
		out := NormalizeForNetwork("devnet", ForNetwork("devnet"))
		for _, sym := range []string{"SOL", "USDC", "PYUSD"} {
			require.Contains(t, out, sym)
		}
	})

	t.Run("malformed token entries are dropped, never fatal", func(t *testing.T) {
		out := NormalizeForNetwork("mainnet", map[string]config.TokenConfig{
			"":       {Name: "Empty", Mint: "SomeMint"},
			"NOMINT": {Name: "No Mint"},
			"USDC":   {Name: "USD Coin", Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		})
		require.Len(t, out, 1)
		require.Contains(t, out, "USDC")
	})
}
