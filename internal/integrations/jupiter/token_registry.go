package jupiter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const jupiterVerifiedTagURL = "https://api.jup.ag/tokens/v2/tag"

// JupiterToken represents a token from Jupiter Tokens V2.
type JupiterToken struct {
	ID       string   `json:"id"`
	Symbol   string   `json:"symbol"`
	Name     string   `json:"name"`
	Decimals int      `json:"decimals"`
	Icon     string   `json:"icon"`
	Tags     []string `json:"tags"`
}

// ResolvedToken represents a fully resolved token with all metadata.
type ResolvedToken struct {
	Symbol      string // Token symbol (e.g., "USDC", "SOL", "BONK")
	Name        string // Human-readable name
	Decimals    int    // Token decimal places
	MainnetMint string // Mainnet mint address
	DevnetMint  string // Devnet mint address (if known)
	LogoURI     string // Logo URL
	IsVerified  bool   // Whether token is from Jupiter verified list
}

// TokenRegistry holds resolved token metadata.
// Tokens are resolved once at startup and cached forever.
type TokenRegistry struct {
	tokens map[string]ResolvedToken // symbol -> resolved token
	mu     sync.RWMutex
}

// NewTokenRegistry creates an empty token registry.
func NewTokenRegistry() *TokenRegistry {
	return &TokenRegistry{
		tokens: make(map[string]ResolvedToken),
	}
}

// fetchJupiterVerifiedList fetches verified tokens from Jupiter Tokens V2.
func fetchJupiterVerifiedList(ctx context.Context, apiKey string) ([]JupiterToken, error) {
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("jupiter api key is required")
	}

	client := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jupiterVerifiedTagURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	query := req.URL.Query()
	query.Set("query", "verified")
	req.URL.RawQuery = query.Encode()
	req.Header.Set("x-api-key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch Jupiter verified tokens: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Jupiter verified tokens returned status %d", resp.StatusCode)
	}

	var tokens []JupiterToken
	if err := json.NewDecoder(resp.Body).Decode(&tokens); err != nil {
		return nil, fmt.Errorf("failed to decode Jupiter verified tokens: %w", err)
	}

	return tokens, nil
}

// wellKnownDevnetMints maps mainnet token symbols to their devnet equivalents.
// Only includes tokens with well-known devnet faucets.
var wellKnownDevnetMints = map[string]string{
	"SOL":   "So11111111111111111111111111111111111111112",  // Native SOL (same on all networks)
	"USDC":  "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", // USDC devnet faucet
	"PYUSD": "CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM", // PYUSD devnet
	"USDT":  "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", // USDT devnet (may not be available)
}

// defaultEnabledTokens is the default list of tokens when none are specified.
var defaultEnabledTokens = []string{"SOL", "USDC", "PYUSD"}

// LoadFromJupiter fetches verified tokens from Jupiter Tokens V2 and resolves the enabled tokens.
// This should be called once at startup. Returns error if any enabled token cannot be resolved.
func (r *TokenRegistry) LoadFromJupiter(ctx context.Context, apiKey string, enabledTokens []string) error {
	if len(enabledTokens) == 0 {
		enabledTokens = defaultEnabledTokens
		log.WithField("tokens", enabledTokens).Info("Using default enabled tokens")
	}

	// Fetch Jupiter verified tokens
	jupiterTokens, err := fetchJupiterVerifiedList(ctx, apiKey)
	if err != nil {
		return fmt.Errorf("failed to fetch Jupiter verified tokens: %w", err)
	}

	// Build symbol -> JupiterToken lookup (case-insensitive)
	jupiterBySymbol := make(map[string]JupiterToken, len(jupiterTokens))
	for _, t := range jupiterTokens {
		key := strings.ToUpper(t.Symbol)
		// Prefer the first occurrence (usually the most canonical one)
		if _, exists := jupiterBySymbol[key]; !exists {
			jupiterBySymbol[key] = t
		}
	}

	log.WithField("count", len(jupiterTokens)).Info("Loaded Jupiter verified token list")

	// Resolve each enabled token
	r.mu.Lock()
	defer r.mu.Unlock()

	resolved := make(map[string]ResolvedToken, len(enabledTokens))
	var failedTokens []string

	for _, symbol := range enabledTokens {
		symbolUpper := strings.ToUpper(strings.TrimSpace(symbol))
		if symbolUpper == "" {
			continue
		}

		// Look up in Jupiter verified list
		jupToken, found := jupiterBySymbol[symbolUpper]
		if !found {
			failedTokens = append(failedTokens, symbol)
			continue
		}

		// Resolve devnet mint if known
		devnetMint := wellKnownDevnetMints[symbolUpper]

		resolved[symbolUpper] = ResolvedToken{
			Symbol:      jupToken.Symbol,
			Name:        jupToken.Name,
			Decimals:    jupToken.Decimals,
			MainnetMint: jupToken.ID,
			DevnetMint:  devnetMint,
			LogoURI:     jupToken.Icon,
			IsVerified:  true,
		}

		log.WithFields(log.Fields{
			"symbol":   jupToken.Symbol,
			"name":     jupToken.Name,
			"mint":     jupToken.ID,
			"decimals": jupToken.Decimals,
		}).Debug("Resolved token from Jupiter")
	}

	if len(failedTokens) > 0 {
		return fmt.Errorf("failed to resolve tokens from Jupiter verified list: %v (tokens must be verified on Jupiter)", failedTokens)
	}

	r.tokens = resolved
	log.WithField("count", len(resolved)).Info("Token registry initialized")

	return nil
}

// LoadFromConfig loads tokens from the legacy SupportedTokens config format.
// This is used for backwards compatibility when enabled_tokens is not set.
func (r *TokenRegistry) LoadFromConfig(tokens map[string]struct {
	Symbol      string
	Name        string
	Mint        string
	MainnetMint string
	Decimals    int
	Enabled     bool
}) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tokens = make(map[string]ResolvedToken)

	for symbol, t := range tokens {
		if !t.Enabled {
			continue
		}

		mint := t.MainnetMint
		if mint == "" {
			mint = t.Mint
		}

		devnetMint := wellKnownDevnetMints[strings.ToUpper(symbol)]

		r.tokens[strings.ToUpper(symbol)] = ResolvedToken{
			Symbol:      t.Symbol,
			Name:        t.Name,
			Decimals:    t.Decimals,
			MainnetMint: mint,
			DevnetMint:  devnetMint,
			LogoURI:     "",
			IsVerified:  false,
		}
	}

	log.WithField("count", len(r.tokens)).Info("Token registry loaded from config")
}

// Get returns a token by symbol. Returns (token, true) if found.
func (r *TokenRegistry) Get(symbol string) (ResolvedToken, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	token, ok := r.tokens[strings.ToUpper(symbol)]
	return token, ok
}

// GetMint returns the appropriate mint address for a token based on network.
func (r *TokenRegistry) GetMint(symbol string, isDevnet bool) (string, bool) {
	token, ok := r.Get(symbol)
	if !ok {
		return "", false
	}

	if isDevnet && token.DevnetMint != "" {
		return token.DevnetMint, true
	}
	return token.MainnetMint, true
}

// All returns all resolved tokens.
func (r *TokenRegistry) All() map[string]ResolvedToken {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string]ResolvedToken, len(r.tokens))
	for k, v := range r.tokens {
		result[k] = v
	}
	return result
}

// Symbols returns all enabled token symbols.
func (r *TokenRegistry) Symbols() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	symbols := make([]string, 0, len(r.tokens))
	for symbol := range r.tokens {
		symbols = append(symbols, symbol)
	}
	return symbols
}

// Count returns the number of registered tokens.
func (r *TokenRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.tokens)
}
