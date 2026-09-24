package tokens

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

const (
	usdtMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"
	eurcMint = "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr"
	usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
)

// #360: a feed wins; otherwise parity is granted only to a registry MINT with a
// USD peg (a symbol merely named like a stablecoin gets nothing); non-USD pegs
// and unknown tokens are disabled.
func TestClassifyPricing(t *testing.T) {
	for _, tc := range []struct {
		symbol, mint string
		want         TokenPricing
		wantPegOf    string
	}{
		{"USDC", usdcMint, TokenPricingFeed, "USDC"},
		{"sol", "So11111111111111111111111111111111111111112", TokenPricingFeed, ""},
		{"TETHER", usdtMint, TokenPricingUSDParity, "USDT"},
		{"EURC", eurcMint, TokenPricingDisabled, "EURC"},
		{"EURC", "FakeMint1111111111111111111111111111111111", TokenPricingDisabled, ""},
		{"NOPE", "NopeMint1111111111111111111111111111111111", TokenPricingDisabled, ""},
	} {
		got, sc := ClassifyPricing(tc.symbol, tc.mint)
		require.Equal(t, tc.want, got, tc.symbol)
		require.Equal(t, tc.wantPegOf, sc.Symbol, tc.symbol)
	}
	for symbol, token := range DefaultSupportedTokens() {
		got, _ := ClassifyPricing(symbol, token.Mint)
		require.Equal(t, TokenPricingFeed, got, "every shipped mainnet token is feed-backed (depeg-protected): %s", symbol)
		require.False(t, IsFeedlessStablecoin(symbol), symbol)
	}

	require.True(t, IsStablecoin(" usdt "))
	require.False(t, IsStablecoin("EURC"), "EUR-pegged")
	require.False(t, IsStablecoin("SOL"))
	require.True(t, IsUSDPeggedToken("WLFI-DOLLAR", "USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB"))
	require.False(t, IsUSDPeggedToken("EURC", eurcMint))
}

// #360 degrade-not-die: normalization never fails; it drops what cannot be
// priced on mainnet and applies no pricing policy on devnet.
func TestNormalizeForNetwork(t *testing.T) {
	out := NormalizeForNetwork("mainnet", map[string]config.TokenConfig{
		"usdc":   {Mint: usdcMint},
		"MYUSD":  {Name: "Tether alias", Mint: usdtMint},
		"EURC":   {Mint: eurcMint},
		"NOPE":   {Mint: "NopeMint1111111111111111111111111111111111"},
		"NOMINT": {Name: "No Mint"},
		" ":      {Mint: usdcMint},
	})
	require.Equal(t, map[string]config.TokenConfig{
		"USDC":  {Name: "USDC", Mint: usdcMint},
		"MYUSD": {Name: "Tether alias", Mint: usdtMint},
	}, out)

	out = NormalizeForNetwork("devnet", map[string]config.TokenConfig{"WEIRD": {Mint: "WeirdMint1111111111111111111111111111111111"}})
	require.Contains(t, out, "WEIRD")

	for _, network := range []string{"mainnet", "devnet"} {
		require.Len(t, NormalizeForNetwork(network, ForNetwork(network)), len(ForNetwork(network)), network)
	}
}

// or#881 select-and-restrict: the declared set IS the accepted set; registry
// symbols take the registry mint (restating it is refused, even correctly);
// custom symbols need their mint; nothing declared means USDC alone.
func TestResolveDeclared(t *testing.T) {
	mainnet, devnet := DefaultSupportedTokens(), DefaultDevnetTokens()

	out, err := ResolveDeclared("mainnet", map[string]config.TokenConfig{"usdc": {}, "SOL": {}})
	require.NoError(t, err)
	require.Equal(t, map[string]config.TokenConfig{"USDC": mainnet["USDC"], "SOL": mainnet["SOL"]}, out)

	out, err = ResolveDeclared("mainnet", map[string]config.TokenConfig{"USDC": {Name: "Dollars"}})
	require.NoError(t, err)
	require.Equal(t, config.TokenConfig{Name: "Dollars", Mint: mainnet["USDC"].Mint}, out["USDC"])

	out, err = ResolveDeclared("devnet", map[string]config.TokenConfig{"USDC": {}})
	require.NoError(t, err)
	require.Equal(t, devnet["USDC"], out["USDC"])

	for _, network := range []string{"mainnet", "devnet"} {
		out, err = ResolveDeclared(network, nil)
		require.NoError(t, err)
		require.Equal(t, map[string]config.TokenConfig{"USDC": ForNetwork(network)["USDC"]}, out)
	}

	const custom = "MyTk1111111111111111111111111111111111111111"
	out, err = ResolveDeclared("mainnet", map[string]config.TokenConfig{"mytk": {Name: "My Token", Mint: custom}})
	require.NoError(t, err)
	require.Equal(t, map[string]config.TokenConfig{"MYTK": {Name: "My Token", Mint: custom}}, out)

	// USDT has no devnet deployment, so on devnet it is an ad-hoc token.
	out, err = ResolveDeclared("devnet", map[string]config.TokenConfig{"USDT": {Mint: custom}})
	require.NoError(t, err)
	require.Equal(t, custom, out["USDT"].Mint)

	for _, tc := range []struct {
		network  string
		declared map[string]config.TokenConfig
		want     string
	}{
		{"mainnet", map[string]config.TokenConfig{"USDC": {Mint: mainnet["USDC"].Mint}}, "remove tokens.USDC.mint"},
		{"mainnet", map[string]config.TokenConfig{"USDC": {Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1w"}}, "built-in token"},
		{"mainnet", map[string]config.TokenConfig{"USDT": {Mint: custom}}, "built-in token"},
		{"mainnet", map[string]config.TokenConfig{"MYTK": {Name: "My Token"}}, "MYTK requires mint"},
		{"devnet", map[string]config.TokenConfig{"USDT": {}}, "not a built-in token on devnet"},
		{"mainnet", map[string]config.TokenConfig{"  ": {Mint: "x"}}, "empty symbol"},
	} {
		_, err := ResolveDeclared(tc.network, tc.declared)
		require.ErrorContains(t, err, tc.want)
	}
}
