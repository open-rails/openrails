package railresolve

import (
	"testing"

	"github.com/open-rails/openrails/internal/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/stretchr/testify/require"
)

// or#881: the declared tokens map IS the accepted set; built-in symbols take the
// registry mint (restating it fails closed); mainnet pricing policy only subtracts.
func TestSolanaRailConfigFromSettings(t *testing.T) {
	const customMint = "MyTk1111111111111111111111111111111111111111"
	declared := config.SolanaAccountSettings{Tokens: map[string]config.TokenConfig{"USDC": {Name: "Dollars"}, "MYTK": {Name: "My Token", Mint: customMint}}}

	out, err := SolanaRailConfigFromSettings(declared, true)
	require.NoError(t, err)
	require.Equal(t, "devnet", out.Network)
	require.Len(t, out.Tokens, 2, "undeclared registry tokens (SOL, PYUSD) must not be accepted")
	require.Equal(t, solanatokens.ForNetwork("devnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Equal(t, "Dollars", out.Tokens["USDC"].Name)
	require.Equal(t, customMint, out.Tokens["MYTK"].Mint)

	out, err = SolanaRailConfigFromSettings(declared, false)
	require.NoError(t, err)
	require.Equal(t, solanatokens.ForNetwork("mainnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Len(t, out.Tokens, 1, "feedless non-stablecoin is disabled on mainnet")

	_, err = SolanaRailConfigFromSettings(config.SolanaAccountSettings{Tokens: map[string]config.TokenConfig{"USDC": {Mint: "Ovr11111111111111111111111111111111111111111"}}}, true)
	require.ErrorContains(t, err, "USDC is a built-in token")

	for testMode, network := range map[bool]string{true: "devnet", false: "mainnet"} {
		out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{}, testMode)
		require.NoError(t, err)
		require.Equal(t, map[string]config.TokenConfig{"USDC": solanatokens.ForNetwork(network)["USDC"]}, out.Tokens, "%s default is USDC alone", network)
	}
}
