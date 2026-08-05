package railresolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// or#881 select-and-restrict: the merchant's declared `tokens` map IS the
// accepted set, and a built-in symbol's mint comes from the registry rather than
// being re-typed (every re-typed mint is a chance to paste a wrong address on a
// money path). Declaring nothing accepts USDC alone.

func TestSolanaRailConfigFromSettings_DeclaredSetIsTheAcceptedSet(t *testing.T) {
	const customMint = "MyTk1111111111111111111111111111111111111111"

	out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{
			"USDC": {},
			"MYTK": {Name: "My Token", Mint: customMint},
		},
	}, true) // devnet: no pricing requirements, so nothing is dropped for price reasons
	require.NoError(t, err)

	require.Equal(t, solanatokens.ForNetwork("devnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Equal(t, customMint, out.Tokens["MYTK"].Mint)
	require.Len(t, out.Tokens, 2, "undeclared registry tokens must not be accepted")
	for _, refused := range []string{"SOL", "PYUSD"} {
		require.NotContainsf(t, out.Tokens, refused, "%s was never declared", refused)
	}
}

// The #360 pricing policy still applies on top of the restriction, and it can
// only ever REMOVE a declared token — never add an undeclared one.
func TestSolanaRailConfigFromSettings_MainnetPricingPolicyOnlySubtracts(t *testing.T) {
	out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{
		Tokens: map[string]config.TokenConfig{
			"USDC": {},
			"MYTK": {Mint: "MyTk1111111111111111111111111111111111111111"},
		},
	}, false)
	require.NoError(t, err)

	require.Equal(t, solanatokens.ForNetwork("mainnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.NotContains(t, out.Tokens, "MYTK", "feedless non-stablecoin is disabled by the pricing policy")
	require.Len(t, out.Tokens, 1)
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

// Declaring nothing accepts USDC alone — the zero-configuration default, chosen
// because it is the one token on every network that also rebills.
func TestSolanaRailConfigFromSettings_NoDeclarationIsUSDCOnly(t *testing.T) {
	for _, testMode := range []bool{true, false} {
		network := "mainnet"
		if testMode {
			network = "devnet"
		}
		out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{}, testMode)
		require.NoError(t, err)
		require.Equalf(t, map[string]config.TokenConfig{
			"USDC": solanatokens.ForNetwork(network)["USDC"],
		}, out.Tokens, "%s default accepted set", network)
	}
}
