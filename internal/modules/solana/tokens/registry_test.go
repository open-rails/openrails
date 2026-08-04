package tokens

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
)

// registryDecimals pins the base-unit precision each registry mint reports
// on-chain, verified 2026-08-04 via getAccountInfo (jsonParsed) against
// api.mainnet-beta.solana.com / api.devnet.solana.com. The runtime NEVER reads
// this table — decimals come from the mint at conversion time (#817) — it exists
// so a mistyped registry address is caught here: a wrong address does not have
// these decimals, and most do not decode as a mint at all.
var registryDecimals = map[string]int{
	// mainnet
	"So11111111111111111111111111111111111111112":  9, // SOL   (Tokenkeg)
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": 6, // USDC  (Tokenkeg)
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": 6, // USDT  (Tokenkeg)
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": 6, // PYUSD (Token-2022)
	"USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB":  6, // USD1  (Tokenkeg)
	"2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH": 6, // USDG  (Token-2022)
	// devnet
	"4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU": 6, // USDC devnet
	"CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM": 6, // PYUSD devnet
}

// fakeMint serves the recorded on-chain layout for a registry mint. Tests never
// touch a real RPC; this is the injectable seam solanaint.ReadMintDecimals reads
// through in production.
type fakeMint map[string]int

func (f fakeMint) GetAccountData(_ context.Context, address solanago.PublicKey) ([]byte, error) {
	decimals, ok := f[address.String()]
	if !ok {
		return nil, nil // absent account: ReadMintDecimals must fail closed
	}
	data := make([]byte, solanaint.MintAccountSize)
	data[44] = byte(decimals)
	data[45] = 1 // is_initialized
	return data, nil
}

// or#881: every address in the registry must be a real, decodable mint pubkey
// whose on-chain precision is payable. The registry is the ONE place a mint is
// written down now, so a typo here is not caught by a merchant's review.
func TestRegistryMintsAreValidAndPayable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reader := fakeMint(registryDecimals)

	for _, network := range []string{"mainnet", "devnet"} {
		for symbol, token := range ForNetwork(network) {
			pk, err := solanago.PublicKeyFromBase58(token.Mint)
			require.NoErrorf(t, err, "%s/%s mint is not a valid base58 pubkey", network, symbol)

			decimals, err := solanaint.ReadMintDecimals(ctx, reader, pk)
			require.NoErrorf(t, err, "%s/%s mint %s is not in the verified set", network, symbol, token.Mint)
			require.NoErrorf(t, config.ValidateTokenDecimals(symbol, decimals),
				"%s/%s on-chain decimals are not payable", network, symbol)
		}
	}
}

// The stablecoin peg registry and the mint registry both write down a mint.
// They must never disagree: the peg registry is the trust anchor for the $1.00
// parity grant, so a drifted copy would grant parity to the wrong mint.
func TestStablecoinMintsAgreeWithRegistry(t *testing.T) {
	t.Parallel()
	mainnet := DefaultSupportedTokens()
	for _, sc := range knownStablecoins {
		token, ok := mainnet[sc.Symbol]
		if !ok {
			continue // a known peg that is not an accepted token (EURC)
		}
		require.Equalf(t, token.Mint, sc.Mint, "%s mint disagrees between the two registries", sc.Symbol)
	}
}

// Devnet mints are a separate column, never the mainnet address under another
// name — SOL is the one deliberate exception (same native-mint pubkey).
func TestDevnetRegistryDoesNotReuseMainnetMints(t *testing.T) {
	t.Parallel()
	mainnet := DefaultSupportedTokens()
	for symbol, token := range DefaultDevnetTokens() {
		if symbol == "SOL" {
			require.Equal(t, mainnet["SOL"].Mint, token.Mint, "native SOL shares one mint across networks")
			continue
		}
		require.NotEqualf(t, mainnet[symbol].Mint, token.Mint, "%s devnet mint must not be the mainnet address", symbol)
	}
}
