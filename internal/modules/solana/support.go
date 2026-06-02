package solana

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/fx"
)

func RequireSolanaProcessorConfig(cfg *config.Config) (*config.ProcessorConfig, error) {
	if cfg == nil {
		return nil, fmt.Errorf("solana not configured")
	}
	proc := cfg.GetSolanaProcessor()
	if proc == nil {
		return nil, fmt.Errorf("solana not configured")
	}
	return proc, nil
}

const WrappedSOLMint = "So11111111111111111111111111111111111111112"

func IsNativeSOLMint(tokenMint string) bool {
	mint := strings.TrimSpace(tokenMint)
	return mint == "" || strings.EqualFold(mint, WrappedSOLMint)
}

// TokenQuote represents a complete quote for converting fiat to a Solana token.
// It includes all the information needed to audit and verify the quote.
type TokenQuote struct {
	Units         uint64
	Decimal       float64
	TokenPriceUSD float64
	FXRate        float64
	FXCurrency    string
	AmountUSD     float64
	QuotedAt      time.Time
}

type TokenPriceProvider interface {
	PriceUSD(ctx context.Context, symbol string) (float64, error)
}

// CalculateTokenQuote converts a fiat amount into token units based on live prices.
func CalculateTokenQuote(ctx context.Context, tokenSymbol string, tokenCfg config.SolanaToken, amountCents int64, currency string, fxProvider fx.Provider, priceProvider TokenPriceProvider) (*TokenQuote, error) {
	if amountCents <= 0 {
		return &TokenQuote{Units: 0, Decimal: 0, FXRate: 1.0, FXCurrency: strings.ToLower(currency), QuotedAt: time.Now()}, nil
	}
	tokenSymbol = strings.ToUpper(strings.TrimSpace(tokenSymbol))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("token symbol is required")
	}

	currency = strings.ToLower(strings.TrimSpace(currency))
	if currency == "" {
		currency = "usd"
	}

	quotedAt := time.Now()
	amountInCurrency := float64(amountCents) / math.Pow10(currencyMinorUnits(currency))

	var amountUSD float64
	var fxRate float64
	if currency == "usd" {
		amountUSD = amountInCurrency
		fxRate = 1.0
	} else {
		if fxProvider == nil {
			return nil, fmt.Errorf("FX conversion required for currency %s but no FX provider configured", currency)
		}
		fxQuote, err := fxProvider.QuoteToUSD(ctx, currency)
		if err != nil {
			return nil, fmt.Errorf("failed to get FX rate for %s: %w", currency, err)
		}
		fxRate = fxQuote.Rate
		amountUSD = amountInCurrency * fxRate
	}

	if strings.TrimSpace(tokenCfg.Mint) == "" {
		return nil, fmt.Errorf("token %s missing mint configuration", tokenSymbol)
	}
	// Feedless stablecoins (USD1/USDG/BUIDL) are pegged to $1.00 and have no Pyth
	// feed; price them directly. USDC/PYUSD keep their configured Pyth feeds.
	tokenPriceUSD := 1.0
	if !config.IsFeedlessStablecoin(tokenSymbol) {
		if priceProvider == nil {
			return nil, fmt.Errorf("token price provider is not configured")
		}
		p, err := priceProvider.PriceUSD(ctx, tokenSymbol)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch token price: %w", err)
		}
		if p <= 0 {
			return nil, fmt.Errorf("token price unavailable for %s", tokenSymbol)
		}
		tokenPriceUSD = p
	}

	scale := math.Pow10(tokenCfg.Decimals)
	tokenAmountFloat := amountUSD / tokenPriceUSD
	tokenUnits := uint64(math.Ceil(tokenAmountFloat * scale))
	tokenDecimal := float64(tokenUnits) / scale

	return &TokenQuote{
		Units:         tokenUnits,
		Decimal:       tokenDecimal,
		TokenPriceUSD: tokenPriceUSD,
		FXRate:        fxRate,
		FXCurrency:    currency,
		AmountUSD:     amountUSD,
		QuotedAt:      quotedAt,
	}, nil
}

func currencyMinorUnits(currency string) int {
	switch strings.ToLower(strings.TrimSpace(currency)) {
	case "bhd", "jod", "kwd", "omr", "tnd":
		return 3
	case "bif", "clp", "djf", "gnf", "isk", "jpy", "kmf", "krw", "pyg", "rwf", "ugx", "vnd", "vuv", "xaf", "xof", "xpf":
		return 0
	default:
		return 2
	}
}
