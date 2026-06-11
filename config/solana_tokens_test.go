package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #360: every default mainnet token MUST be coverable by the pricing policy —
// either a built-in Pyth feed or a USD-pegged registry entry. This is the
// config-level regression for the doujins crash-loop ("solana token USD1
// missing pyth price feed"): defaults and the feed map must never diverge into
// a fatal state again.
func TestDefaultMainnetTokensAllPriceable(t *testing.T) {
	t.Parallel()
	for symbol, token := range DefaultSupportedTokens() {
		decision, _ := ClassifySolanaTokenPricing(symbol, token.Mint)
		require.NotEqual(t, TokenPricingDisabled, decision,
			"default mainnet token %s must be priceable (feed or USD parity)", symbol)
	}
	// USD1/USDG specifically graduated from feedless to feed-backed with
	// verified Pyth feed ids.
	for _, symbol := range []string{"USD1", "USDG"} {
		decision, _ := ClassifySolanaTokenPricing(symbol, DefaultSupportedTokens()[symbol].Mint)
		require.Equal(t, TokenPricingFeed, decision, "%s has a verified Pyth feed", symbol)
		require.False(t, IsFeedlessStablecoin(symbol), "%s is no longer feedless", symbol)
	}
}

func TestClassifySolanaTokenPricing(t *testing.T) {
	t.Parallel()

	t.Run("feed-backed symbol uses the feed", func(t *testing.T) {
		decision, _ := ClassifySolanaTokenPricing("USDC", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
		require.Equal(t, TokenPricingFeed, decision)
	})

	t.Run("USD-pegged registry mint without feed degrades to parity", func(t *testing.T) {
		decision, sc := ClassifySolanaTokenPricing("USDT", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
		require.Equal(t, TokenPricingUSDParity, decision)
		require.Equal(t, "usd", sc.Peg)
	})

	t.Run("non-USD-pegged stablecoin without feed is disabled", func(t *testing.T) {
		decision, sc := ClassifySolanaTokenPricing("EURC", "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr")
		require.Equal(t, TokenPricingDisabled, decision)
		require.Equal(t, "eur", sc.Peg)
	})

	t.Run("unknown token without feed is disabled", func(t *testing.T) {
		decision, sc := ClassifySolanaTokenPricing("NOPE", "NopeMint1111111111111111111111111111111111")
		require.Equal(t, TokenPricingDisabled, decision)
		require.Empty(t, sc.Symbol)
	})

	t.Run("parity is mint-anchored: stablecoin SYMBOL with unknown mint gets no parity", func(t *testing.T) {
		decision, _ := ClassifySolanaTokenPricing("USDT", "FakeMint1111111111111111111111111111111111")
		require.Equal(t, TokenPricingDisabled, decision)
	})

	t.Run("custom symbol pointing at a known USD mint gets parity", func(t *testing.T) {
		decision, sc := ClassifySolanaTokenPricing("TETHER", "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
		require.Equal(t, TokenPricingUSDParity, decision)
		require.Equal(t, "USDT", sc.Symbol)
	})
}

func TestStablecoinRegistryHelpers(t *testing.T) {
	t.Parallel()

	require.True(t, IsStablecoin("usdc"))
	require.True(t, IsStablecoin("USD1"))
	require.True(t, IsStablecoin("USDG"))
	require.True(t, IsStablecoin("USDT"))
	require.False(t, IsStablecoin("EURC"), "EURC is EUR-pegged, not a USD stablecoin")
	require.False(t, IsStablecoin("SOL"))

	require.True(t, IsUSDPeggedToken("WLFI-DOLLAR", "USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB"),
		"custom symbol with known USD mint is USD-pegged")
	require.False(t, IsUSDPeggedToken("EURC", "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"))

	sc, ok := KnownStablecoinByMint("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH")
	require.True(t, ok)
	require.Equal(t, "USDG", sc.Symbol)

	_, ok = KnownStablecoinByMint("So11111111111111111111111111111111111111112")
	require.False(t, ok)

	// USDT remains the only feedless USD stablecoin shipped today.
	require.True(t, IsFeedlessStablecoin("USDT"))
	require.False(t, IsFeedlessStablecoin("USDC"))
	require.False(t, IsFeedlessStablecoin("EURC"))
}

// #360 Validate-ordering pin: a solana processor with unpriceable tokens must
// pass Validate (warn-only) — the runtime pass disables tokens; the container
// always boots.
func TestValidateSolanaProcessorNeverFatal(t *testing.T) {
	t.Parallel()

	cfg := GetDefaultBillingConfig()
	cfg.DB.URL = "postgres://admin:admin_password@localhost:5432/openrails_db?sslmode=disable"
	cfg.Mode = ModeFull
	cfg.Processors = map[string]*ProcessorConfig{
		"solana": {
			Tokens: map[string]TokenConfig{
				"NOPE": {Name: "Mystery", Mint: "NopeMint1111111111111111111111111111111111", Decimals: 6},
				"EURC": {Name: "Euro Coin", Mint: "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr", Decimals: 6},
				"":     {Name: "Empty", Mint: "x", Decimals: 6},
			},
		},
	}
	require.NoError(t, Validate(cfg))

	// And under test_env (devnet) there is no pricing requirement at all.
	cfg.TestEnv = true
	require.NoError(t, Validate(cfg))
}
