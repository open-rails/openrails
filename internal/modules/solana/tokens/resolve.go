package tokens

import (
	"strings"

	"github.com/open-rails/openrails/internal/config"
)

// DefaultAcceptedSymbol is the token a merchant accepts when they declare
// nothing. Only USDC: it is on every network and on the recurring allowlist, so
// a zero-config merchant can take one-off payments and rebill. Anything more is
// an explicit merchant choice.
const DefaultAcceptedSymbol = PreferredStablecoin

// ResolveDeclared turns a merchant's declared token map into the accepted token
// set for network. The declared set IS the accepted set; nothing declared means
// DefaultAcceptedSymbol. The registry only resolves a known symbol's on-chain
// identity, never what is accepted.
//
//   - A registry symbol is selected by name (`tokens: {USDC: {}}`) and takes
//     its mint from ForNetwork. Declaring `mint:` for it is an error even when
//     it agrees: a typed mint can be wrong and accept a different token than
//     the one priced.
//   - A custom symbol requires `mint:`, its identity.
//
// On devnet, a registry symbol without a devnet entry is declared like any
// ad-hoc token, with an explicit mint: devnet mints have no canonical address
// to protect and devnet money is fake.
func ResolveDeclared(network string, declared map[string]config.TokenConfig) (map[string]config.TokenConfig, error) {
	registry := ForNetwork(network)
	if len(declared) == 0 {
		return map[string]config.TokenConfig{DefaultAcceptedSymbol: registry[DefaultAcceptedSymbol]}, nil
	}
	out := make(map[string]config.TokenConfig, len(declared))
	for rawSymbol, token := range declared {
		symbol := strings.ToUpper(strings.TrimSpace(rawSymbol))
		if symbol == "" {
			return nil, errEmptySymbol
		}
		mint := strings.TrimSpace(token.Mint)
		known, isRegistry := registry[symbol]
		switch {
		case isRegistry && mint != "":
			return nil, &DeclaredRegistryMintError{Symbol: symbol, Network: normalizeNetwork(network), RegistryMint: known.Mint}
		case isRegistry:
			if name := strings.TrimSpace(token.Name); name != "" {
				known.Name = name
			}
			out[symbol] = known
		case mint == "":
			return nil, &MissingMintError{Symbol: symbol, Network: normalizeNetwork(network)}
		default:
			token.Mint = mint
			out[symbol] = token
		}
	}
	return out, nil
}

// DeclaredRegistryMintError: a built-in token's mint was restated in config.
type DeclaredRegistryMintError struct {
	Symbol       string
	Network      string
	RegistryMint string
}

func (e *DeclaredRegistryMintError) Error() string {
	return "solana tokens: " + e.Symbol + " is a built-in token — remove tokens." + e.Symbol +
		".mint and declare `" + e.Symbol + ": {}`; its " + e.Network + " mint comes from the registry (" + e.RegistryMint + ")"
}

// MissingMintError: a custom token was named without the mint that identifies it.
type MissingMintError struct {
	Symbol  string
	Network string
}

func (e *MissingMintError) Error() string {
	return "solana tokens: " + e.Symbol + " requires mint — it is not a built-in token on " + e.Network
}

type emptySymbolError struct{}

func (emptySymbolError) Error() string { return "solana tokens: token declared with an empty symbol" }

var errEmptySymbol = emptySymbolError{}

func normalizeNetwork(network string) string {
	if strings.EqualFold(strings.TrimSpace(network), "devnet") {
		return "devnet"
	}
	return "mainnet"
}
