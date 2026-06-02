// Package recurring holds the OpenRails service layer for Solana recurring
// subscriptions (issues #254/#255/#256/#257): plan publishing, enrollment, the
// cyclical pull, and dunning — built on the on-chain instruction builders in
// internal/integrations/solana/subscriptions and the per-tenant Signer.
//
// It is intentionally separate from the one-off Solana Pay flow in
// internal/modules/solana so the two payment shapes evolve independently.
package recurring

import (
	"fmt"
	"slices"
	"strings"

	"github.com/open-rails/openrails/config"
)

// RecurringStablecoins is the launch allowlist of token SYMBOLS eligible to back
// a recurring Solana subscription.
//
// USDC only at launch. PYUSD is intentionally excluded pending the #252 devnet
// spike: its Token-2022 mint has PermanentDelegate + TransferFee extensions
// initialized, both on the Subscriptions program's reject list, so create_plan
// almost certainly rejects it (and mint extensions are immutable). USDT is
// excluded permanently for regulatory/counterparty-freeze risk. Volatile tokens
// (SOL, etc.) are excluded because on-chain plan amounts are immutable — only a
// stablecoin keeps a fixed base-unit amount ≈ a fixed USD amount across cycles.
//
// One-off purchases are unaffected — they accept the full DefaultSupportedTokens
// set and FX-quote at purchase time.
var RecurringStablecoins = []string{"USDC"}

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
// its mint + decimals for the given network (mainnet/devnet). It fails closed:
// an unknown token, an off-allowlist token (USDT/SOL/PYUSD-for-now), or a token
// without a configured mint for the network all return an error, so a caller can
// never publish a plan against an ineligible or misconfigured mint.
func ResolveRecurringMint(symbol, network string) (mint string, decimals int, err error) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return "", 0, fmt.Errorf("recurring: token symbol is required")
	}
	if !IsRecurringStablecoinSymbol(sym) {
		return "", 0, ErrTokenNotRecurringEligible{Symbol: sym}
	}
	tokens := config.TokensForNetwork(network)
	tok, ok := tokens[sym]
	if !ok || strings.TrimSpace(tok.Mint) == "" {
		return "", 0, fmt.Errorf("recurring: token %q has no configured mint on network %q", sym, network)
	}
	return tok.Mint, tok.Decimals, nil
}

// ValidateRecurringMint confirms that the supplied mint matches the configured
// mint for symbol on network AND that symbol is recurring-eligible. Use this when
// a caller supplies both a symbol and a mint (defense against a mismatched or
// spoofed mint at plan-create time).
func ValidateRecurringMint(symbol, mint, network string) error {
	want, _, err := ResolveRecurringMint(symbol, network)
	if err != nil {
		return err
	}
	if strings.TrimSpace(mint) != want {
		return fmt.Errorf("recurring: mint %q does not match the configured %s mint %q on network %q",
			mint, strings.ToUpper(strings.TrimSpace(symbol)), want, network)
	}
	return nil
}
