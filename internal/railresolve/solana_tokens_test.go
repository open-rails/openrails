package railresolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// or#881: a declared `tokens` map used to REPLACE the curated registry
// wholesale, so adding one custom token forced the merchant to re-type every
// canonical mint they still wanted. Every re-typed mint is a chance to paste a
// wrong address on a money path. A declaration now extends/overrides the
// curated set per symbol.

func TestSolanaRailConfigFromSettings_CustomTokenExtendsCuratedRegistry(t *testing.T) {
	const customMint = "MyTk1111111111111111111111111111111111111111"

	out := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{
			"MYTK": {Name: "My Token", Mint: customMint},
		},
	}, true) // devnet: no pricing requirements, so nothing is dropped for price reasons

	curated := solanatokens.ForNetwork("devnet")
	require.NotEmpty(t, curated)
	for symbol, want := range curated {
		got, ok := out.Tokens[symbol]
		require.Truef(t, ok, "curated token %s was dropped by declaring one custom token", symbol)
		require.Equalf(t, want.Mint, got.Mint, "curated token %s mint changed", symbol)
	}
	require.Equal(t, customMint, out.Tokens["MYTK"].Mint)
	require.Len(t, out.Tokens, len(curated)+1)
}

func TestSolanaRailConfigFromSettings_MainnetCustomTokenKeepsCuratedMints(t *testing.T) {
	out := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{
			// A custom token with no Pyth feed is disabled by the #360 pricing
			// policy — but that must never take the curated set with it.
			"MYTK": {Mint: "MyTk1111111111111111111111111111111111111111"},
		},
	}, false)

	for symbol, want := range solanatokens.ForNetwork("mainnet") {
		got, ok := out.Tokens[symbol]
		require.Truef(t, ok, "curated mainnet token %s was dropped", symbol)
		require.Equalf(t, want.Mint, got.Mint, "curated mainnet token %s mint changed", symbol)
	}
	require.NotContains(t, out.Tokens, "MYTK", "feedless non-stablecoin should still be disabled")
}

// A declaration for a curated symbol overrides that ONE entry and leaves the
// rest of the registry alone.
func TestSolanaRailConfigFromSettings_DeclarationOverridesOneSymbol(t *testing.T) {
	const overrideMint = "Ovr11111111111111111111111111111111111111111"

	out := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{"USDC": {Mint: overrideMint}},
	}, true)

	require.Equal(t, overrideMint, out.Tokens["USDC"].Mint)
	curated := solanatokens.ForNetwork("devnet")
	for symbol, want := range curated {
		if symbol == "USDC" {
			continue
		}
		require.Equalf(t, want.Mint, out.Tokens[symbol].Mint, "curated token %s changed", symbol)
	}
	require.Len(t, out.Tokens, len(curated))
}

// Declaring nothing still yields exactly the curated set.
func TestSolanaRailConfigFromSettings_NoDeclarationIsCuratedSet(t *testing.T) {
	out := SolanaRailConfigFromSettings(config.SolanaAccountSettings{}, true)
	require.Equal(t, solanatokens.NormalizeForNetwork("devnet", solanatokens.ForNetwork("devnet")), out.Tokens)
}
