package solana

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	log "github.com/sirupsen/logrus"
)

// stablecoinPegTolerance is the band around $1.00 within which a stablecoin's peg
// is used verbatim (no sub-penny noise). Outside it (a real depeg), the live
// price is used as a failsafe — 1%, rarely triggered.
const stablecoinPegTolerance = 0.01

// stablecoinPriceUSD returns the USD price for a stablecoin: $1.00 by default, or
// the live price when a feed shows the peg has diverged by more than
// stablecoinPegTolerance (the failsafe — e.g. a $0.95 USDC yields a ~5% higher
// token charge so the merchant still nets the USD amount). Feedless stablecoins,
// a nil provider, or an unavailable/invalid feed all fall back to the $1.00 peg.
func stablecoinPriceUSD(ctx context.Context, symbol string, priceProvider TokenPriceProvider) float64 {
	if priceProvider == nil || solanatokens.IsFeedlessStablecoin(symbol) {
		return 1.0
	}
	p, err := priceProvider.PriceUSD(ctx, symbol)
	if err != nil || p <= 0 {
		log.WithField("token", symbol).WithError(err).Warn("stablecoin price feed unavailable; using $1.00 peg")
		return 1.0
	}
	if math.Abs(p-1.0) <= stablecoinPegTolerance {
		return 1.0
	}
	log.WithFields(log.Fields{"token": symbol, "price_usd": p}).
		Warn("stablecoin depeg beyond tolerance; using live price (failsafe)")
	return p
}

// USDCDecimals is the SPL decimal precision for USDC (and the other $1-pegged
// stablecoins OpenRails recurring-bills in): 6. One USD cent therefore equals
// 10^6 / 100 = 10_000 base units at the $1 peg.
const USDCDecimals = 6

// FiatCentsToStablecoinBaseUnits converts a fiat-cent amount into stablecoin
// base units for a $1-pegged token (#267). At the peg, 1 cent = 10_000 base
// units (10^6 / 100). The same depeg failsafe used for quotes applies: if a
// live feed shows the stablecoin has diverged beyond stablecoinPegTolerance, the
// charge is scaled up by 1/price so the merchant still nets the USD amount.
//
// Negative cents clamp to 0. The result rounds UP (ceil) so a partial base unit
// never under-charges the merchant.
func FiatCentsToStablecoinBaseUnits(ctx context.Context, cents int64, symbol string, priceProvider TokenPriceProvider) uint64 {
	if cents <= 0 {
		return 0
	}
	priceUSD := stablecoinPriceUSD(ctx, symbol, priceProvider)
	if priceUSD <= 0 {
		priceUSD = 1.0
	}
	// USD value of the charge / price-per-token => token amount, then to base units.
	usd := float64(cents) / 100.0
	scale := math.Pow10(USDCDecimals)
	return uint64(math.Ceil((usd / priceUSD) * scale))
}

func RequireSolanaProcessorConfig(processors config.ProcessorSet) (*config.ProcessorConfig, error) {
	if processors == nil {
		return nil, fmt.Errorf("solana not configured")
	}
	proc := processors.GetSolanaProcessor()
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
func CalculateTokenQuote(ctx context.Context, tokenSymbol string, tokenCfg config.TokenConfig, amountCents int64, currency string, fxProvider fx.Provider, priceProvider TokenPriceProvider) (*TokenQuote, error) {
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
	// USD-pegged stablecoins are treated as $1.00 (no sub-penny price noise). A
	// divergence failsafe (rarely triggered) consults the price feed when one
	// exists: if the stablecoin has depegged beyond the tolerance, the live price
	// is used so the charge compensates. The check is mint-aware (#360) so a
	// custom-symbol token pointing at a known USD-pegged mint still gets parity
	// pricing. Everything else (SOL, EURC, ...) always requires a live price.
	var tokenPriceUSD float64
	if solanatokens.IsUSDPeggedToken(tokenSymbol, tokenCfg.Mint) {
		tokenPriceUSD = stablecoinPriceUSD(ctx, tokenSymbol, priceProvider)
	} else {
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
