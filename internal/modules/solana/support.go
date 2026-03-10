package solana

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/fx"
	jupiter "github.com/open-rails/openrails/internal/integrations/jupiter"
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

// CalculateTokenQuote converts a fiat amount into token units based on live prices.
func CalculateTokenQuote(ctx context.Context, tokenCfg config.SolanaToken, amountCents int64, currency string, fxProvider fx.Provider) (*TokenQuote, error) {
	if amountCents <= 0 {
		return &TokenQuote{Units: 0, Decimal: 0, FXRate: 1.0, FXCurrency: strings.ToLower(currency), QuotedAt: time.Now()}, nil
	}

	currency = strings.ToLower(strings.TrimSpace(currency))
	if currency == "" {
		currency = "usd"
	}

	quotedAt := time.Now()
	amountInCurrency := float64(amountCents) / 100.0

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

	mint := tokenCfg.MainnetMint
	if mint == "" {
		mint = tokenCfg.Mint
	}
	if mint == "" {
		return nil, fmt.Errorf("token %s missing mint configuration", tokenCfg.Symbol)
	}

	prices, err := jupiter.FetchJupiterPrices(ctx, []string{mint})
	if err != nil {
		return nil, fmt.Errorf("failed to fetch token price: %w", err)
	}

	tokenPriceUSD, ok := prices[mint]
	if !ok || tokenPriceUSD <= 0 {
		return nil, fmt.Errorf("token price unavailable for %s", tokenCfg.Symbol)
	}
	tokenPriceUSD = normalizeStablecoinPrice(tokenCfg.Symbol, tokenPriceUSD)

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

var stablecoinSymbols = map[string]bool{
	"USDC":  true,
	"USDT":  true,
	"PYUSD": true,
	"USDP":  true,
	"BUSD":  true,
	"DAI":   true,
}

const stablecoinTolerance = 0.02

func normalizeStablecoinPrice(symbol string, price float64) float64 {
	if !stablecoinSymbols[strings.ToUpper(symbol)] {
		return price
	}
	if price >= (1.0-stablecoinTolerance) && price <= (1.0+stablecoinTolerance) {
		return 1.0
	}
	return price
}
