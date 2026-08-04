package tokens

import (
	"strings"

	"github.com/open-rails/openrails/config"
)

// ResolveDeclared turns a merchant's DECLARED token map into the accepted token
// set for network (or#881).
//
// Select-and-restrict, not restate:
//
//   - A registry symbol is SELECTED by name — `tokens: {USDC: {}}`. Its mint
//     comes from ForNetwork(network). Declaring `mint:` for it is an ERROR even
//     when the address agrees: a value a merchant can type is a value they can
//     get wrong, and a wrong mint here accepts payment in a different token than
//     the one that was priced.
//   - A custom symbol still REQUIRES `mint:` — there the mint IS the token's
//     identity, so it is genuinely declared rather than restated.
//   - Declarations EXTEND the registry per symbol; they never replace it, so
//     adding one custom token cannot silently drop the canonical set.
//
// TEST MODE (devnet): the registry has its own devnet column, so USDC/SOL/PYUSD
// select identically there. Registry symbols with no devnet entry are not
// registry symbols ON DEVNET — a devnet deployment that needs one declares it
// like any other ad-hoc token, with an explicit mint. That is deliberate:
// devnet mints are per-deployment artefacts with no canonical address to
// protect, and devnet money is fake (the whole #360 pricing policy is skipped
// there), so the restatement hazard the rule exists to prevent does not apply.
func ResolveDeclared(network string, declared map[string]config.TokenConfig) (map[string]config.TokenConfig, error) {
	registry := ForNetwork(network)
	out := make(map[string]config.TokenConfig, len(registry)+len(declared))
	for symbol, token := range registry {
		out[symbol] = token
	}
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
