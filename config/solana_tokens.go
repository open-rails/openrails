package config

import "strings"

type SolanaToken = TokenConfig

const (
	DefaultPythHermesURL        = "https://hermes.pyth.network"
	DefaultPythMaxPriceAge      = "2m"
	DefaultPythMaxConfidenceBPS = 100

	PythFeedSOLUSD   = "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d"
	PythFeedUSDCUSD  = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a"
	PythFeedPYUSDUSD = "c1da1b73d7f01e7ddd54b3766cf7fcd644395ad14f70aa706ec5384c59e76692"
)

func DefaultPythPriceFeeds() map[string]string {
	return map[string]string{
		"SOL":   PythFeedSOLUSD,
		"USDC":  PythFeedUSDCUSD,
		"PYUSD": PythFeedPYUSDUSD,
	}
}

func DefaultSupportedTokens() map[string]SolanaToken {
	return map[string]SolanaToken{
		"SOL": {
			Name:     "Solana",
			Mint:     "So11111111111111111111111111111111111111112",
			Decimals: 9,
		},
		"USDC": {
			Name:     "USD Coin",
			Mint:     "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
			Decimals: 6,
		},
		"PYUSD": {
			Name:     "PayPal USD",
			Mint:     "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
			Decimals: 6,
		},
		// USD1 (World Liberty Financial USD) — plain SPL Token mint, no
		// extensions, so it is recurring-eligible (verified by mint inspection,
		// like USDC). Mainnet only; no devnet mint exists.
		"USD1": {
			Name:     "World Liberty Financial USD",
			Mint:     "USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB",
			Decimals: 6,
		},
		// USDG (Global Dollar, Paxos) is a Token-2022 mint with extensions the
		// Subscriptions program rejects, so it is supported for ONE-OFF purchases
		// only (NOT recurring). Mainnet only.
		"USDG": {
			Name:     "Global Dollar",
			Mint:     "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH",
			Decimals: 6,
		},
	}
}

// stablecoins are USD-pegged tokens quoted at $1.00 (with a divergence failsafe;
// see CalculateTokenQuote). USDC is the preferred default for the frontend.
var stablecoins = map[string]bool{
	"USDC":  true,
	"PYUSD": true,
	"USD1":  true,
	"USDG":  true,
}

// feedlessStablecoins are stablecoins with NO market price feed, so their peg
// can't be divergence-checked — they are always priced at $1.00. USDC/PYUSD have
// Pyth feeds and so can be failsafe-checked for a depeg.
var feedlessStablecoins = map[string]bool{
	"USD1": true,
	"USDG": true,
}

// PreferredStablecoin is the default stablecoin the frontend presents first for
// Solana purchase options.
const PreferredStablecoin = "USDC"

// IsStablecoin reports whether symbol is a USD-pegged stablecoin.
func IsStablecoin(symbol string) bool {
	return stablecoins[strings.ToUpper(strings.TrimSpace(symbol))]
}

// IsFeedlessStablecoin reports whether symbol is a stablecoin with no price feed
// (always priced at the $1.00 peg).
func IsFeedlessStablecoin(symbol string) bool {
	return feedlessStablecoins[strings.ToUpper(strings.TrimSpace(symbol))]
}

func DefaultDevnetTokens() map[string]SolanaToken {
	return map[string]SolanaToken{
		"SOL": {
			Name:     "Solana",
			Mint:     "So11111111111111111111111111111111111111112",
			Decimals: 9,
		},
		"USDC": {
			Name:     "USD Coin (Devnet)",
			Mint:     "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU",
			Decimals: 6,
		},
		"PYUSD": {
			Name:     "PayPal USD (Devnet)",
			Mint:     "CXk2AMBfi3TwaEL2468s6zP8xq9NxTXjp9gjMgzeUynM",
			Decimals: 6,
		},
	}
}

func GetTokenBySymbol(symbol string, useDevnet bool) (SolanaToken, bool) {
	var network string
	if useDevnet {
		network = "devnet"
	} else {
		network = "mainnet"
	}
	tokens := TokensForNetwork(network)
	token, exists := tokens[symbol]
	return token, exists
}

func IsValidToken(symbol string) bool {
	_, exists := DefaultSupportedTokens()[symbol]
	return exists
}

func TokensForNetwork(network string) map[string]SolanaToken {
	switch strings.ToLower(network) {
	case "devnet":
		return DefaultDevnetTokens()
	default:
		return DefaultSupportedTokens()
	}
}
