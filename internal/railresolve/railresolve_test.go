package railresolve

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// A Solana PSP's environment is its network, where it accepts exactly SOL and
// USDC, SOL alone on testnet.
func TestSolanaRailConfig(t *testing.T) {
	for network, want := range map[string][]string{"mainnet": {"SOL", "USDC"}, "devnet": {"SOL", "USDC"}, "testnet": {"SOL"}} {
		out := SolanaRailConfig(network, "https://rpc.example/k")
		require.Equal(t, network, out.Network)
		require.Equal(t, "https://rpc.example/k", out.RPCURL)
		require.Len(t, out.Tokens, len(want), network)
		for _, symbol := range want {
			require.Equal(t, solanatokens.Accepted(network)[symbol], out.Tokens[symbol], "%s %s", network, symbol)
		}
	}
	require.Equal(t, solanatokens.DevnetUSDCMint, SolanaRailConfig(config.EnvironmentDevnet, "").Tokens["USDC"].Mint)
	require.Equal(t, solanatokens.MainnetUSDCMint, SolanaRailConfig(config.EnvironmentMainnet, "").Tokens["USDC"].Mint)
}
