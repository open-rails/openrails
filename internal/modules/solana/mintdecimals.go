package solana

import (
	"context"
	"fmt"
	"strings"
	"sync"

	solanago "github.com/gagliardetto/solana-go"

	"github.com/open-rails/openrails/internal/config"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/railresolve"
)

// MintDecimalsSource resolves a mint's base-unit precision. The real
// implementation reads the SPL mint account: decimals come from the chain,
// never from merchant config.
type MintDecimalsSource interface {
	ForMint(ctx context.Context, mint string) (int, error)
}

// MintDecimals reads SPL mint decimals from the chain and memoizes them per
// merchant + mint (merchants may be armed on different clusters). Caching is
// sound only because decimals are immutable; mutable accounts (balances, plan
// state) are re-read, slot-gated after our own confirmed writes.
type MintDecimals struct {
	reader solanarpc.MintAccountReader

	mu    sync.RWMutex
	cache map[string]int
}

// NewMintDecimals wires a cached mint-decimals resolver over a chain reader.
func NewMintDecimals(reader solanarpc.MintAccountReader) *MintDecimals {
	return &MintDecimals{reader: reader, cache: make(map[string]int)}
}

func mintCacheKey(ctx context.Context, mint string) string {
	if mid, ok := merchant.FromContext(ctx); ok {
		return mid.String() + "|" + mint
	}
	return "|" + mint
}

// ForMint returns the mint's on-chain base-unit precision, validated as usable
// for payment amounts. Fails closed: an unarmed reader, an unreadable mint, or a
// precision outside the payable range are errors — never a fabricated 6.
func (m *MintDecimals) ForMint(ctx context.Context, mint string) (int, error) {
	mint = strings.TrimSpace(mint)
	if mint == "" {
		return 0, fmt.Errorf("solana: mint address is required to resolve decimals")
	}
	if m == nil {
		return 0, fmt.Errorf("solana: no mint-decimals resolver armed (mint %s)", mint)
	}
	key := mintCacheKey(ctx, mint)

	m.mu.RLock()
	cached, ok := m.cache[key]
	m.mu.RUnlock()
	if ok {
		return cached, nil
	}

	pk, err := solanago.PublicKeyFromBase58(mint)
	if err != nil {
		return 0, fmt.Errorf("solana: invalid mint address %q: %w", mint, err)
	}
	decimals, err := solanarpc.ReadMintDecimals(ctx, m.reader, pk)
	if err != nil {
		return 0, err
	}
	if err := config.ValidateTokenDecimals(mint, decimals); err != nil {
		return 0, err
	}

	m.mu.Lock()
	m.cache[key] = decimals
	m.mu.Unlock()
	return decimals, nil
}

// RequireMintDecimals reads a mint's on-chain decimals through an armed
// resolver. An unarmed resolver is an error; there is no default.
func RequireMintDecimals(ctx context.Context, mints MintDecimalsSource, mint string) (int, error) {
	if mints == nil {
		return 0, fmt.Errorf("solana: no mint-decimals resolver armed (mint %s)", mint)
	}
	return mints.ForMint(ctx, mint)
}

// ResolveTokenMint returns the merchant's configured mint address for symbol.
// The mint is merchant configuration; its decimals are not.
func ResolveTokenMint(ctx context.Context, src railresolve.Source, symbol string) (string, error) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return "", fmt.Errorf("solana: token symbol is required")
	}
	proc, err := RequireSolanaRailConfig(ctx, src)
	if err != nil {
		return "", err
	}
	if proc.Solana == nil {
		return "", fmt.Errorf("solana rail is not configured")
	}
	tok, ok := proc.Solana.Tokens[sym]
	if !ok {
		return "", fmt.Errorf("solana: token %s is not configured for this merchant", sym)
	}
	mint := strings.TrimSpace(tok.Mint)
	if mint == "" {
		return "", fmt.Errorf("solana: token %s has no configured mint", sym)
	}
	return mint, nil
}

// RequireTokenDecimals resolves the ctx merchant's configured mint for symbol
// and reads that mint's decimals FROM THE CHAIN. Fails closed at every step: an
// unarmed rail, an unknown token, a mintless token, or an unreadable mint are
// all errors — guessing the scale is a 10^n mispricing.
func RequireTokenDecimals(ctx context.Context, src railresolve.Source, symbol string, mints MintDecimalsSource) (int, error) {
	mint, err := ResolveTokenMint(ctx, src, symbol)
	if err != nil {
		return 0, err
	}
	if mints == nil {
		return 0, fmt.Errorf("solana: no mint-decimals resolver armed for token %s", symbol)
	}
	return mints.ForMint(ctx, mint)
}
