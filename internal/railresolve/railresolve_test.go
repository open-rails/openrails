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

	out, err := SolanaRailConfigFromSettings(declared, config.EnvironmentSandbox)
	require.NoError(t, err)
	require.Equal(t, "devnet", out.Network)
	require.Len(t, out.Tokens, 2, "undeclared registry tokens (SOL, PYUSD) must not be accepted")
	require.Equal(t, solanatokens.ForNetwork("devnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Equal(t, "Dollars", out.Tokens["USDC"].Name)
	require.Equal(t, customMint, out.Tokens["MYTK"].Mint)

	out, err = SolanaRailConfigFromSettings(declared, config.EnvironmentLive)
	require.NoError(t, err)
	require.Equal(t, solanatokens.ForNetwork("mainnet")["USDC"].Mint, out.Tokens["USDC"].Mint)
	require.Len(t, out.Tokens, 1, "feedless non-stablecoin is disabled on mainnet")

	_, err = SolanaRailConfigFromSettings(config.SolanaAccountSettings{Tokens: map[string]config.TokenConfig{"USDC": {Mint: "Ovr11111111111111111111111111111111111111111"}}}, config.EnvironmentSandbox)
	require.ErrorContains(t, err, "USDC is a built-in token")

	for environment, network := range map[string]string{config.EnvironmentSandbox: "devnet", config.EnvironmentLive: "mainnet"} {
		out, err := SolanaRailConfigFromSettings(config.SolanaAccountSettings{}, environment)
		require.NoError(t, err)
		require.Equal(t, network, out.Network)
		registry := solanatokens.ForNetwork(network)
		require.Equal(t, map[string]config.TokenConfig{"SOL": registry["SOL"], "USDC": registry["USDC"]}, out.Tokens, "%s default is SOL and USDC", network)
	}
}
