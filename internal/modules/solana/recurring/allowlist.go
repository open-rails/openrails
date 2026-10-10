// Package recurring is the service layer for Solana recurring subscriptions on
// the subscriptions program: plan publishing, enrollment, the pull crank and
// dunning. It is separate from the one-off Solana Pay flow.
package recurring

import (
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// RecurringStablecoins is the allowlist of token symbols that may back a
// recurring Solana subscription. The subscriptions program rejects mints with
// ConfidentialTransfer, NonTransferable, PermanentDelegate, TransferHook,
// TransferFee, MintCloseAuthority or Pausable extensions:
//
//   - USDC, USD1 (mainnet only), DUSD (devnet test coin): plain SPL Token.
//   - USDT: plain SPL Token, but no devnet mint to verify create_plan against.
//   - PYUSD, USDG: Token-2022 with PermanentDelegate+TransferFee, rejected.
//   - SOL and volatile tokens: plan amounts are immutable, so only a stablecoin
//     keeps a fixed USD value across cycles.
//
// Mint extensions are immutable, so a rejected token never becomes eligible.
// One-off purchases are unaffected.
var RecurringStablecoins = []string{"USDC", "USD1", "DUSD"}

// IsRecurringStablecoinSymbol reports whether symbol is on the recurring allowlist.
func IsRecurringStablecoinSymbol(symbol string) bool {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	return slices.Contains(RecurringStablecoins, s)
}

// ErrTokenNotRecurringEligible is returned when a non-allowlisted token is used
// to publish a recurring plan.
type ErrTokenNotRecurringEligible struct {
	Symbol string
}

func (e ErrTokenNotRecurringEligible) Error() string {
	return fmt.Sprintf("token %q is not eligible for recurring Solana subscriptions (allowed: %s)",
		e.Symbol, strings.Join(RecurringStablecoins, ", "))
}

// ResolveRecurringMint returns an allowlisted symbol's built-in mint for network
// (mainnet/devnet), failing closed on an unknown, ineligible or unconfigured
// token. Decimals come from the mint on-chain.
func ResolveRecurringMint(symbol, network string) (mint string, err error) {
	return ResolveRecurringMintFromTokens(symbol, solanatokens.ForNetwork(network))
}

// ResolveRecurringMintFromTokens is ResolveRecurringMint against the runtime
// token config, the source of truth for production; the built-in network
// defaults serve tests only.
func ResolveRecurringMintFromTokens(symbol string, tokens map[string]config.TokenConfig) (mint string, err error) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return "", fmt.Errorf("recurring: token symbol is required")
	}
	if !IsRecurringStablecoinSymbol(sym) {
		return "", ErrTokenNotRecurringEligible{Symbol: sym}
	}
	tok, ok := tokens[sym]
	if !ok || strings.TrimSpace(tok.Mint) == "" {
		return "", fmt.Errorf("recurring: token %q has no configured mint", sym)
	}
	return tok.Mint, nil
}

func normalizeRecurringTokens(tokens map[string]config.TokenConfig) map[string]config.TokenConfig {
	normalized := make(map[string]config.TokenConfig, len(tokens))
	for symbol, token := range tokens {
		s := strings.ToUpper(strings.TrimSpace(symbol))
		if s == "" {
			continue
		}
		normalized[s] = token
	}
	return normalized
}

func firstTokenMap(tokens []map[string]config.TokenConfig) map[string]config.TokenConfig {
	if len(tokens) == 0 {
		return nil
	}
	return tokens[0]
}
