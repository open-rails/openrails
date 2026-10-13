// Package tokens is the fixed set of tokens OpenRails accepts on Solana: SOL
// and USDC, each at its live price, on the network a PSP's environment names.
package tokens

import (
	"strings"

	"github.com/open-rails/openrails/internal/config"
)

const (
	DefaultPythHermesURL        = "https://hermes.pyth.network"
	DefaultPythMaxPriceAge      = "2m"
	DefaultPythMaxConfidenceBPS = 100

	PythFeedSOLUSD  = "ef0d8b6fda2ceba41da15d4095d1da392a0d2f8ed0c6c7bc0f4cfac8c280b56d"
	PythFeedUSDCUSD = "eaa020c61cc479712813461ce153894a96a6c00b21ed0cfc2798d1f9a9e9c94a"
)

// The tokens OpenRails accepts.
const (
	SOL  = "SOL"
	USDC = "USDC"
	// PreferredStablecoin is the token the payment page presents first and
	// the one a recurring plan bills in.
	PreferredStablecoin = USDC
)

// Mints.
const (
	SOLMint         = "So11111111111111111111111111111111111111112"
	MainnetUSDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	DevnetUSDCMint  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
)

// DefaultPythPriceFeeds are the live price feeds, by symbol. USDC's guards
// its peg.
func DefaultPythPriceFeeds() map[string]string {
	return map[string]string{SOL: PythFeedSOLUSD, USDC: PythFeedUSDCUSD}
}

// Accepted is what a Solana PSP accepts on network, by symbol: SOL and USDC on
// mainnet and devnet, SOL alone on testnet, which has no canonical USDC. The
// map is a fresh copy.
func Accepted(network string) map[string]config.TokenConfig {
	out := map[string]config.TokenConfig{SOL: {Name: "Solana", Mint: SOLMint}}
	switch strings.ToLower(strings.TrimSpace(network)) {
	case config.EnvironmentMainnet:
		out[USDC] = config.TokenConfig{Name: "USD Coin", Mint: MainnetUSDCMint}
	case config.EnvironmentDevnet:
		out[USDC] = config.TokenConfig{Name: "USD Coin (Devnet)", Mint: DevnetUSDCMint}
	}
	return out
}

// IsUSDC reports whether symbol on mint is USDC: the one stablecoin, priced at
// its $1.00 peg unless its feed shows a depeg.
func IsUSDC(symbol, mint string) bool {
	mint = strings.TrimSpace(mint)
	return strings.EqualFold(strings.TrimSpace(symbol), USDC) && (mint == MainnetUSDCMint || mint == DevnetUSDCMint)
}
