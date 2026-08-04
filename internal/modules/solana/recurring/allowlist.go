// Package recurring holds the OpenRails service layer for Solana recurring
// subscriptions (issues #254/#255/#256/#257): plan publishing, enrollment, the
// cyclical pull, and dunning — built on the on-chain instruction builders in
// internal/integrations/solana/subscriptions and the per-merchant Signer.
//
// It is intentionally separate from the one-off Solana Pay flow in
// internal/modules/solana so the two payment shapes evolve independently.
package recurring

import (
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/openrails/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// RecurringStablecoins is the launch allowlist of token SYMBOLS eligible to back
// a recurring Solana subscription.
//
// Eligibility is determined by the mint's token-program + extension set (the
// Subscriptions program rejects mints carrying ConfidentialTransfer,
// NonTransferable, PermanentDelegate, TransferHook, TransferFee,
// MintCloseAuthority, or Pausable). Verified per token by on-chain mint
// inspection (and create_plan on devnet for USDC):
//
//   - USDC  — plain SPL Token, no extensions → eligible (create_plan ACCEPTED on devnet).
//   - USD1  — plain SPL Token, no extensions → eligible (World Liberty Financial USD; mainnet only).
//   - USDT  — plain SPL Token, so extension-eligible, but NOT allowlisted: no
//     devnet deployment exists to run create_plan against, so it stays one-off
//     until that verification is done.
//   - PYUSD — Token-2022 w/ PermanentDelegate+TransferFee → REJECTED (devnet error 121 mintHasPermanentDelegate).
//   - USDG  — Token-2022 w/ PermanentDelegate+TransferFee+ConfidentialTransfer+TransferHook → rejected.
//   - SOL / volatile — excluded: on-chain plan amounts are immutable, so only a
//     stablecoin keeps a fixed base-unit amount ≈ a fixed USD amount across cycles.
//
// Mint extensions are immutable, so a rejected token can never become eligible.
// One-off purchases are unaffected — they accept the full solanatokens defaults
// set and FX-quote at purchase time.
var RecurringStablecoins = []string{"USDC", "USD1"}

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

// ResolveRecurringMint validates that symbol is recurring-eligible and returns
// its mint for the given network (mainnet/devnet). It fails closed: an unknown
// token, an off-allowlist token (PYUSD/USDG/SOL), or a token without a
// configured mint for the network all return an error, so a caller can never
// publish a plan against an ineligible or misconfigured mint. Decimals are NOT
// returned — they come from the mint on-chain (#817).
func ResolveRecurringMint(symbol, network string) (mint string, err error) {
	return ResolveRecurringMintFromTokens(symbol, solanatokens.ForNetwork(network))
}

// ResolveRecurringMintFromTokens validates that symbol is recurring-eligible and
// resolves it from the runtime-configured token map. Production callers must use
// this so deployed token config is the source of truth; hard-coded network
// defaults are only used by legacy tests/helpers that call ResolveRecurringMint.
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
