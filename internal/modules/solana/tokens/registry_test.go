package tokens

import (
	"context"
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
)

// verifiedDecimals is each registry mint's on-chain precision, verified
// 2026-08-04 via getAccountInfo on mainnet-beta/devnet. The runtime never reads
// it (#817); it exists so a mistyped registry address fails here.
var verifiedDecimals = map[string]int{
	"So11111111111111111111111111111111111111112":  9, // SOL
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": 6, // USDC
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": 6, // USDT
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": 6, // PYUSD (Token-2022)
	"USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB":  6, // USD1
	"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH": 6, // USDG (Token-2022)
	"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU": 6, // USDC devnet
	"CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM": 6, // PYUSD devnet
	"7R5ehi23KtGj8e5ysBjr39dktJh2KtSFSeH44fd2s22T": 6, // DUSD devnet
}

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

// or#881: the registry is the one place a mint is written down. Every entry is
// a verified, payable mint; the peg registry (the $1 parity trust anchor)
// agrees with it; devnet never reuses a mainnet address except native SOL; and
// the zero-config default exists on every network as a USD stablecoin.
func TestRegistryIntegrity(t *testing.T) {
	mainnet, devnet := DefaultSupportedTokens(), DefaultDevnetTokens()
	for network, set := range map[string]map[string]config.TokenConfig{"mainnet": mainnet, "devnet": devnet} {
		require.Equal(t, set, ForNetwork(network))
		for symbol, token := range set {
			pk, err := solanago.PublicKeyFromBase58(token.Mint)
			require.NoError(t, err, "%s/%s", network, symbol)
			d, err := solanaint.ReadMintDecimals(context.Background(), verifiedMints(verifiedDecimals), pk)
			require.NoError(t, err, "%s/%s mint %s is not in the verified set", network, symbol, token.Mint)
			require.NoError(t, config.ValidateTokenDecimals(symbol, d))
		}
		require.NotEmpty(t, set[DefaultAcceptedSymbol].Mint, network)
	}
	require.True(t, IsStablecoin(DefaultAcceptedSymbol))

	for _, sc := range knownStablecoins {
		if token, ok := mainnet[sc.Symbol]; ok {
			require.Equal(t, token.Mint, sc.Mint, "%s disagrees between registries", sc.Symbol)
		}
		got, ok := KnownStablecoinByMint(" " + sc.Mint + " ")
		require.True(t, ok, sc.Symbol)
		require.Equal(t, sc, got)
		_, ok = KnownStablecoinByMint(strings.ToLower(sc.Mint))
		require.False(t, ok, "%s: base58 is case-sensitive; a case-folded mint is another address", sc.Symbol)
	}
	for symbol, token := range devnet {
		if symbol == "SOL" {
			require.Equal(t, mainnet["SOL"].Mint, token.Mint)
			continue
		}
		require.NotEqual(t, mainnet[symbol].Mint, token.Mint, symbol)
	}
}
