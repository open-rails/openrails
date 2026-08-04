package railresolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// or#881: a declared `tokens` map used to REPLACE the built-in registry
// wholesale, so adding one custom token forced the merchant to re-type every
// canonical mint they still wanted. Every re-typed mint is a chance to paste a
// wrong address on a money path. A declaration now extends the registry per
// symbol, and a built-in symbol's mint cannot be declared at all.

func TestSolanaRailConfigFromSettings_CustomTokenExtendsRegistry(t *testing.T) {
	const customMint = "MyTk1111111111111111111111111111111111111111"

	out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{
			"MYTK": {Name: "My Token", Mint: customMint},
		},
	}, true) // devnet: no pricing requirements, so nothing is dropped for price reasons
	require.NoError(t, err)

	registry := solanatokens.ForNetwork("devnet")
	require.NotEmpty(t, registry)
	for symbol, want := range registry {
		got, ok := out.Tokens[symbol]
		require.Truef(t, ok, "registry token %s was dropped by declaring one custom token", symbol)
		require.Equalf(t, want.Mint, got.Mint, "registry token %s mint changed", symbol)
	}
	require.Equal(t, customMint, out.Tokens["MYTK"].Mint)
	require.Len(t, out.Tokens, len(registry)+1)
}

func TestSolanaRailConfigFromSettings_MainnetCustomTokenKeepsRegistryMints(t *testing.T) {
	out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{
			// A custom token with no Pyth feed is disabled by the #360 pricing
			// policy — but that must never take the registry with it.
			"MYTK": {Mint: "MyTk1111111111111111111111111111111111111111"},
		},
	}, false)
	require.NoError(t, err)

	for symbol, want := range solanatokens.ForNetwork("mainnet") {
		got, ok := out.Tokens[symbol]
		require.Truef(t, ok, "registry mainnet token %s was dropped", symbol)
		require.Equalf(t, want.Mint, got.Mint, "registry mainnet token %s mint changed", symbol)
	}
	require.NotContains(t, out.Tokens, "MYTK", "feedless non-stablecoin should still be disabled")
}

// Selecting a built-in symbol takes its mint from the registry; restating that
// mint is fail-closed — the PSP does not arm.
func TestSolanaRailConfigFromSettings_BuiltInSymbolIsSelectedNotRestated(t *testing.T) {
	out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{"USDC": {Name: "Dollars"}},
	}, true)
	require.NoError(t, err)
	require.Equal(t, solanatokens.ForNetwork("devnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Equal(t, "Dollars", out.Tokens["USDC"].Name)

	_, err = SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{"USDC": {Mint: "Ovr11111111111111111111111111111111111111111"}},
	}, true)
	require.ErrorContains(t, err, "USDC is a built-in token")
}

// Declaring nothing still yields exactly the registry set.
func TestSolanaRailConfigFromSettings_NoDeclarationIsTheRegistry(t *testing.T) {
	out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{}, true)
	require.NoError(t, err)
	require.Equal(t, solanatokens.NormalizeForNetwork("devnet", solanatokens.ForNetwork("devnet")), out.Tokens)
}
