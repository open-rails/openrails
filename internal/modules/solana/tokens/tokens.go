package tokens

import (
	"strings"

	"github.com/open-rails/openrails/internal/config"
)

const (
	DefaultPythHermesURL        = "https://hermes.pyth.network"
	DefaultPythMaxPriceAge      = "2m"
	DefaultPythMaxConfidenceBPS = 100

	PythFeedSOLUSD   = "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d"
	PythFeedUSDCUSD  = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a"
	PythFeedPYUSDUSD = "c1da1b73d7f01e7ddd54b3766cf7fcd644395ad14f70aa706ec5384c59e76692"
	// USD1 (World Liberty Financial USD): Crypto.USD1/USD in Pyth's Hermes
	// registry (hermes.pyth.network/v2/price_feeds?query=USD1).
	PythFeedUSD1USD = "0a2425d43486780990d8b63543029e20556be51fd756cca584212f4d539611d4"
	// USDG (Global Dollar, Paxos): Crypto.USDG/USD in Pyth's Hermes registry
	// (hermes.pyth.network/v2/price_feeds?query=USDG).
	PythFeedUSDGUSD = "daa58c6a3ce7d4b9c46c32a6e646012c17c4a2b24c08dd8c5e476118b855a7da"
	// USDT (Tether): the "TETHER / US DOLLAR" entry in Pyth's Hermes registry
	// (hermes.pyth.network/v2/price_feeds?query=USDT), not the near-namesakes
	// USDT0/USDTB/OUSDT.
	PythFeedUSDTUSD = "2b89b9dc8fdf9f34709a5b106b472f0f39bb6ca9ce04b0fd7f2e971688e2e53b"
)

func DefaultPythPriceFeeds() map[string]string {
	return map[string]string{
		"SOL":   PythFeedSOLUSD,
		"USDC":  PythFeedUSDCUSD,
		"USDT":  PythFeedUSDTUSD,
		"PYUSD": PythFeedPYUSDUSD,
		"USD1":  PythFeedUSD1USD,
		"USDG":  PythFeedUSDGUSD,
	}
}

// DefaultSupportedTokens is the mainnet half of the built-in mint registry, the
// source of truth for a well-known token's on-chain identity. A merchant selects
// a symbol and never types a mint: a wrong mint would accept payment in a
// different token than the one priced. registry_test.go pins each mint's
// decimals.
func DefaultSupportedTokens() map[string]config.TokenConfig {
	return map[string]config.TokenConfig{
		"SOL": {
			Name: "Solana",
			Mint: "So11111111111111111111111111111111111111112",
		},
		"USDC": {
			Name: "USD Coin",
			Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		},
		// Tether USD — plain SPL Token mint (Tokenkeg), no extensions.
		"USDT": {
			Name: "Tether USD",
			Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB",
		},
		"PYUSD": {
			Name: "PayPal USD",
			Mint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
		},
		// USD1 (World Liberty Financial USD) — plain SPL Token mint, no
		// extensions, so it is recurring-eligible (verified by mint inspection,
		// like USDC). Mainnet only; no devnet mint exists.
		"USD1": {
			Name: "World Liberty Financial USD",
			Mint: "USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB",
		},
		// USDG (Global Dollar, Paxos) is a Token-2022 mint with extensions the
		// Subscriptions program rejects, so it is supported for ONE-OFF purchases
		// only (NOT recurring). Mainnet only.
		"USDG": {
			Name: "Global Dollar",
			Mint: "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH",
		},
	}
}

// StablecoinInfo describes a known stablecoin: its canonical Solana mainnet
// mint (the on-chain identity) and the fiat currency it is pegged to.
type StablecoinInfo struct {
	Symbol string
	Mint   string // canonical mainnet mint
	Peg    string // lowercase ISO code of the peg currency ("usd", "eur")
}

// knownStablecoins is the stablecoin registry. With DefaultPythPriceFeeds it
// drives the mainnet pricing policy (ClassifyPricing): a feed is used when one
// exists (for a stablecoin, the depeg protection); a USD-pegged stablecoin
// without one gets $1.00 parity; a non-USD peg (EURC) or unknown token without
// one is disabled. Parity is granted only by mint, so a token merely named like
// a stablecoin cannot buy a $1.00 quote. Mints must agree with the mainnet
// registry (registry_test.go); EURC is a known peg, not an accepted token.
var knownStablecoins = []StablecoinInfo{
	{Symbol: "USDC", Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Peg: "usd"},
	{Symbol: "USDT", Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Peg: "usd"},
	{Symbol: "PYUSD", Mint: "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", Peg: "usd"},
	// World Liberty Financial USD
	{Symbol: "USD1", Mint: "USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB", Peg: "usd"},
	// Global Dollar (Paxos)
	{Symbol: "USDG", Mint: "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", Peg: "usd"},
	// Circle euro stablecoin — pegged to EUR, NOT eligible for USD parity.
	{Symbol: "EURC", Mint: "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr", Peg: "eur"},
}

// KnownStablecoinByMint looks a stablecoin up by its mint address (the
// on-chain identity — the trust anchor for the parity grant).
func KnownStablecoinByMint(mint string) (StablecoinInfo, bool) {
	mint = strings.TrimSpace(mint)
	for _, sc := range knownStablecoins {
		if sc.Mint == mint {
			return sc, true
		}
	}
	return StablecoinInfo{}, false
}

// KnownStablecoinBySymbol looks a stablecoin up by symbol.
func KnownStablecoinBySymbol(symbol string) (StablecoinInfo, bool) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	for _, sc := range knownStablecoins {
		if sc.Symbol == symbol {
			return sc, true
		}
	}
	return StablecoinInfo{}, false
}

// TokenPricing is the pricing decision for one configured mainnet token.
type TokenPricing int

const (
	// TokenPricingFeed: a Pyth feed covers the symbol — live pricing, with the
	// stablecoin depeg failsafe where applicable.
	TokenPricingFeed TokenPricing = iota
	// TokenPricingUSDParity: known USD-pegged stablecoin without a feed —
	// priced at $1.00 parity (no depeg protection), with a loud warning.
	TokenPricingUSDParity
	// TokenPricingDisabled: the token cannot be priced (non-USD peg or unknown
	// token without a feed); it is dropped with a loud warning, never an error.
	TokenPricingDisabled
)

// ClassifyPricing applies the MAINNET pricing policy to one
// configured token. It is never consulted on devnet (test_mode): devnet money
// is fake and has no pricing requirements at all.
func ClassifyPricing(symbol, mint string) (TokenPricing, StablecoinInfo) {
	normalized := strings.ToUpper(strings.TrimSpace(symbol))
	if strings.TrimSpace(DefaultPythPriceFeeds()[normalized]) != "" {
		sc, _ := KnownStablecoinByMint(mint)
		return TokenPricingFeed, sc
	}
	if sc, ok := KnownStablecoinByMint(mint); ok {
		if sc.Peg == "usd" {
			return TokenPricingUSDParity, sc
		}
		return TokenPricingDisabled, sc
	}
	return TokenPricingDisabled, StablecoinInfo{}
}

// PreferredStablecoin is the default stablecoin the frontend presents first for
// Solana purchase options.
const PreferredStablecoin = "USDC"

// IsStablecoin reports whether symbol is a known USD-pegged stablecoin. These
// are quoted at $1.00 with a divergence failsafe when a price feed exists; see
// CalculateTokenQuote. USDC is the preferred default for the frontend.
func IsStablecoin(symbol string) bool {
	sc, ok := KnownStablecoinBySymbol(symbol)
	return ok && sc.Peg == "usd"
}

// IsUSDPeggedToken reports whether a configured token is USD-pegged: either
// its symbol is a known USD stablecoin, or its mint matches a USD-pegged
// registry entry (covers custom symbols pointing at a known mint).
func IsUSDPeggedToken(symbol, mint string) bool {
	if IsStablecoin(symbol) {
		return true
	}
	sc, ok := KnownStablecoinByMint(mint)
	return ok && sc.Peg == "usd"
}

// IsFeedlessStablecoin reports whether symbol is a USD-pegged stablecoin with
// no built-in price feed, so always priced at the $1.00 peg. Adding a feed
// upgrades a coin to depeg-protected pricing automatically.
func IsFeedlessStablecoin(symbol string) bool {
	if !IsStablecoin(symbol) {
		return false
	}
	normalized := strings.ToUpper(strings.TrimSpace(symbol))
	return strings.TrimSpace(DefaultPythPriceFeeds()[normalized]) == ""
}

// DefaultDevnetTokens is the devnet half of the registry. Devnet mints are
// per-deployment, so only widely used ones are pinned; a registry symbol absent
// here (USDT/USD1/USDG) is declared as an ad-hoc token (see ResolveDeclared).
func DefaultDevnetTokens() map[string]config.TokenConfig {
	return map[string]config.TokenConfig{
		"SOL": {
			Name: "Solana",
			Mint: "So11111111111111111111111111111111111111112",
		},
		"USDC": {
			Name: "USD Coin (Devnet)",
			Mint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
		},
		"PYUSD": {
			Name: "PayPal USD (Devnet)",
			Mint: "CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM",
		},
		"DUSD": {
			Name: "Dev USD",
			Mint: "7R5ehi23KtGj8e5ysBjr39dktJh2KtSFSeH44fd2s22T",
		},
	}
}

// ForNetwork returns the built-in registry for a network. The returned map is a
// fresh copy per call, so callers may mutate it.
func ForNetwork(network string) map[string]config.TokenConfig {
	switch strings.ToLower(network) {
	case "devnet":
		return DefaultDevnetTokens()
	default:
		return DefaultSupportedTokens()
	}
}
