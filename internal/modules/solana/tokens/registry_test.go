package tokens

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
)

// verifiedDecimals is each mint's on-chain precision, verified via
// getAccountInfo, so a mistyped address fails here.
var verifiedDecimals = map[string]int{SOLMint: 9, MainnetUSDCMint: 6, DevnetUSDCMint: 6}

type verifiedMints map[string]int

func (v verifiedMints) GetAccountData(_ context.Context, addr solanago.PublicKey) ([]byte, error) {
	d, ok := v[addr.String()]
	if !ok {
		return nil, nil
	}
	data := make([]byte, solanaint.MintAccountSize)
	data[44], data[45] = byte(d), 1
	return data, nil
}

func TestAcceptedTokens(t *testing.T) {
	for network, want := range map[string][]string{"mainnet": {SOL, USDC}, "devnet": {SOL, USDC}, "testnet": {SOL}} {
		set := Accepted(network)
		require.Len(t, set, len(want), network)
		for _, symbol := range want {
			pk, err := solanago.PublicKeyFromBase58(set[symbol].Mint)
			require.NoError(t, err, "%s/%s", network, symbol)
			d, err := solanaint.ReadMintDecimals(context.Background(), verifiedMints(verifiedDecimals), pk)
			require.NoError(t, err, "%s/%s", network, symbol)
			require.NoError(t, config.ValidateTokenDecimals(symbol, d))
			require.NotEmpty(t, DefaultPythPriceFeeds()[symbol], symbol)
		}
	}
	require.NotEqual(t, Accepted("mainnet")[USDC].Mint, Accepted("devnet")[USDC].Mint)
	require.True(t, IsUSDC("usdc", DevnetUSDCMint))
	require.False(t, IsUSDC("USDC", SOLMint))
}
