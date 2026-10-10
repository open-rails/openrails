package solana

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/modules/money"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	log "github.com/sirupsen/logrus"
)

// stablecoinPegTolerance is the band around $1.00 within which a stablecoin's peg
// is used verbatim (no sub-penny noise). Outside it (a real depeg), the live
// price is used as a failsafe — 1%, rarely triggered.
const stablecoinPegTolerance = 0.01

// stablecoinPriceUSD returns a stablecoin's USD price and whether the $1 peg
// holds. pegged=true: the caller converts by pure integer arithmetic. pegged=false
// is the depeg failsafe: the live price is returned so the merchant still nets
// the USD amount. Feedless stablecoins, a nil provider or a bad feed hold the peg.
func stablecoinPriceUSD(ctx context.Context, symbol string, priceProvider TokenPriceProvider) (priceUSD float64, pegged bool) {
	if priceProvider == nil || solanatokens.IsFeedlessStablecoin(symbol) {
		return 1.0, true
	}
	p, err := priceProvider.PriceUSD(ctx, symbol)
	if err != nil || p <= 0 {
		log.WithField("token", symbol).WithError(err).Warn("stablecoin price feed unavailable; using $1.00 peg")
		return 1.0, true
	}
	if math.Abs(p-1.0) <= stablecoinPegTolerance {
		return 1.0, true
	}
	log.WithFields(log.Fields{"token": symbol, "price_usd": p}).
		Warn("stablecoin depeg beyond tolerance; using live price (failsafe)")
	return p, false
}

// microDecimals is the decimal precision of MICROS (millionths) — the scale every
// fiat amount in OpenRails carries.
const microDecimals = 6

// pow10 returns 10^n (n >= 0) as an exact integer.
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// ceilQuo returns ceil(n/d) for n >= 0, d > 0.
func ceilQuo(n, d *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(n, d, new(big.Int))
	if r.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}

// ceilRat returns ceil(r) for r >= 0.
func ceilRat(r *big.Rat) *big.Int {
	return ceilQuo(r.Num(), r.Denom())
}

func baseUnitsToUint64(n *big.Int, symbol string) (uint64, error) {
	if !n.IsUint64() {
		return 0, fmt.Errorf("solana token %s: converted amount %s overflows uint64 base units", symbol, n.String())
	}
	return n.Uint64(), nil
}

// ratFromRate converts a price/FX RATE (legitimately a float) to the exact
// rational of its shortest DECIMAL form — 1.08 becomes 108/100, not the binary
// double 1.0800000000000000710… whose ceiling would add a phantom base unit.
// From here on the arithmetic is exact and no amount touches a float.
func ratFromRate(rate float64, what string) (*big.Rat, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return nil, fmt.Errorf("invalid %s %v", what, rate)
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(rate, 'g', -1, 64))
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("invalid %s %v", what, rate)
	}
	return r, nil
}

// FiatMicrosToBaseUnitsAtPeg converts micro-USD into base units of a $1-pegged
// token: micros * 10^(decimals-6), an exact CEILING divide when decimals < 6 so
// a fractional base unit never under-charges. No float: float64(micros)/1e6*1e6
// is not the identity in IEEE-754.
func FiatMicrosToBaseUnitsAtPeg(micros moneyutil.Micros, symbol string, decimals int) (uint64, error) {
	if err := config.ValidateTokenDecimals(symbol, decimals); err != nil {
		return 0, err
	}
	if micros <= 0 {
		return 0, nil
	}
	n := new(big.Int).SetInt64(int64(micros))
	if decimals >= microDecimals {
		n.Mul(n, pow10(decimals-microDecimals))
	} else {
		n = ceilQuo(n, pow10(microDecimals-decimals))
	}
	return baseUnitsToUint64(n, symbol)
}

// nativeToBaseUnitsAtRate is the depeg/FX branch: base units =
// ceil(amount * fxRate * 10^decimals / (10^nativeDecimals * tokenPriceUSD)), evaluated as an
// exact rational (rates converted to their exact rational value) with a single
// final ceiling — never a float multiply followed by math.Ceil.
func nativeToBaseUnitsAtRate(amount int64, nativeDecimals int, symbol string, decimals int, fxRate, tokenPriceUSD float64) (uint64, error) {
	if err := config.ValidateTokenDecimals(symbol, decimals); err != nil {
		return 0, err
	}
	if amount <= 0 {
		return 0, nil
	}
	fxr, err := ratFromRate(fxRate, "fx rate")
	if err != nil {
		return 0, err
	}
	price, err := ratFromRate(tokenPriceUSD, "token price")
	if err != nil {
		return 0, err
	}
	q := new(big.Rat).SetInt64(amount)
	q.Mul(q, fxr)
	q.Mul(q, new(big.Rat).SetInt(pow10(decimals)))
	q.Quo(q, new(big.Rat).SetInt(pow10(nativeDecimals)))
	q.Quo(q, price)
	return baseUnitsToUint64(ceilRat(q), symbol)
}

// FiatMicrosToStablecoinBaseUnits converts micro-USD into base units of a
// $1-pegged token at the mint's on-chain decimals. At the peg it is an exact
// integer rescale; the depeg failsafe rounds UP so a fractional base unit never
// under-charges the merchant.
func FiatMicrosToStablecoinBaseUnits(ctx context.Context, micros moneyutil.Micros, symbol string, decimals int, priceProvider TokenPriceProvider) (uint64, error) {
	if err := config.ValidateTokenDecimals(symbol, decimals); err != nil {
		return 0, err
	}
	if micros <= 0 {
		return 0, nil
	}
	priceUSD, pegged := stablecoinPriceUSD(ctx, symbol, priceProvider)
	if pegged {
		return FiatMicrosToBaseUnitsAtPeg(micros, symbol, decimals)
	}
	return nativeToBaseUnitsAtRate(int64(micros), microDecimals, symbol, decimals, 1.0, priceUSD)
}

// FormatBaseUnits renders base units as a fixed-point decimal with `decimals`
// fractional digits, pure integer. It is the only Solana amount formatter and
// reaches the Solana Pay `amount=` wire. Fixed precision states the scale, so a
// wrong `decimals` makes the wallet reject the URL instead of transferring a
// 10^n-wrong amount.
func FormatBaseUnits(units uint64, decimals int) string {
	if decimals <= 0 {
		return strconv.FormatUint(units, 10)
	}
	div := pow10(decimals)
	whole, frac := new(big.Int).QuoRem(new(big.Int).SetUint64(units), div, new(big.Int))
	return fmt.Sprintf("%s.%0*s", whole.String(), decimals, frac.String())
}

// RequireSolanaRailConfig resolves the ctx merchant's armed Solana PSP: the
// psps row's settings as runtime Solana config. Unarmed fails closed.
func RequireSolanaRailConfig(ctx context.Context, src railresolve.Source) (*config.ResolvedPSP, error) {
	if src == nil {
		return nil, fmt.Errorf("solana not configured")
	}
	proc, err := src.RailConfig(ctx, string(models.RailSolana), "")
	if err != nil {
		return nil, fmt.Errorf("solana not configured: %w", err)
	}
	return proc, nil
}

const WrappedSOLMint = "So11111111111111111111111111111111111111112"

func IsNativeSOLMint(tokenMint string) bool {
	mint := strings.TrimSpace(tokenMint)
	return mint == "" || mint == WrappedSOLMint
}

// TokenQuote is an auditable fiat-to-token quote. Units is authoritative;
// Amount is its display rendering. Only the rates are floats.
type TokenQuote struct {
	Units         uint64  // base units to transfer (authoritative)
	Amount        string  // display only: Units at the token's decimals
	TokenPriceUSD float64 // rate
	FXRate        float64 // rate
	FXCurrency    string  // the quoted price's currency
	QuotedAt      time.Time
}

type TokenPriceProvider interface {
	PriceUSD(ctx context.Context, symbol string) (float64, error)
}

// CalculateTokenQuote converts registered native currency units to token base
// units. Token-denominated prices pay exactly in that token; fiat prices use FX
// and the token price. quotedAt comes from the owning checkout clock. decimals
// is the mint's on-chain precision (MintDecimals.ForMint), never an assumed 6.
func CalculateTokenQuote(ctx context.Context, tokenSymbol, mint string, decimals int, amountMicros moneyutil.Micros, currency string, fxProvider fx.Provider, priceProvider TokenPriceProvider, quotedAt time.Time) (*TokenQuote, error) {
	if quotedAt.IsZero() {
		return nil, fmt.Errorf("token quote requires its creation time")
	}
	quotedAt = quotedAt.UTC()
	tokenSymbol = strings.ToUpper(strings.TrimSpace(tokenSymbol))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("token symbol is required")
	}
	if err := config.ValidateTokenDecimals(tokenSymbol, decimals); err != nil {
		return nil, err
	}

	// Never invent a currency: the FX leg and the on-chain charge key off it,
	// so an absent or unregistered code is an error, not a "usd" default.
	currency = money.NormalizeCurrency(currency)
	if currency == "" {
		return nil, fmt.Errorf("token quote requires a currency (refusing to default)")
	}
	if err := ValidateQuoteCurrency(currency, tokenSymbol, mint); err != nil {
		return nil, err
	}
	units, _ := moneyutil.LookupCurrency(currency)
	if units.Kind == "crypto" {
		if units.Decimals != decimals {
			return nil, fmt.Errorf("%s mint precision does not match registered denomination", currency)
		}
		if amountMicros < 0 {
			return nil, fmt.Errorf("token amount cannot be negative")
		}
		amount := uint64(amountMicros)
		return &TokenQuote{Units: amount, Amount: FormatBaseUnits(amount, decimals), FXRate: 1, FXCurrency: currency, QuotedAt: quotedAt}, nil
	}
	if amountMicros <= 0 {
		return &TokenQuote{Units: 0, Amount: FormatBaseUnits(0, decimals), FXRate: 1.0, FXCurrency: currency, QuotedAt: quotedAt}, nil
	}

	fxRate := 1.0
	if currency != money.DefaultCurrency {
		if fxProvider == nil {
			return nil, fmt.Errorf("FX conversion required for currency %s but no FX provider configured", currency)
		}
		fxQuote, err := fxProvider.QuoteToUSD(ctx, currency)
		if err != nil {
			return nil, fmt.Errorf("failed to get FX rate for %s: %w", currency, err)
		}
		fxRate = fxQuote.Rate
	}

	if strings.TrimSpace(mint) == "" {
		return nil, fmt.Errorf("token %s missing mint configuration", tokenSymbol)
	}
	// USD-pegged stablecoins are $1.00 unless the feed shows a depeg beyond
	// tolerance, then the live price compensates. The check is mint-aware, so a
	// custom symbol on a known USD-pegged mint gets parity pricing. Everything
	// else (SOL, EURC, ...) requires a live price.
	var tokenPriceUSD float64
	atPeg := false
	if solanatokens.IsUSDPeggedToken(tokenSymbol, mint) {
		tokenPriceUSD, atPeg = stablecoinPriceUSD(ctx, tokenSymbol, priceProvider)
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

	// USD price + held peg => pure integer rescale. Anything else (FX, depeg, a
	// volatile token) goes through the exact-rational rate path.
	var (
		tokenUnits uint64
		err        error
	)
	if atPeg && currency == money.DefaultCurrency {
		tokenUnits, err = FiatMicrosToBaseUnitsAtPeg(amountMicros, tokenSymbol, decimals)
	} else {
		tokenUnits, err = nativeToBaseUnitsAtRate(int64(amountMicros), units.Decimals, tokenSymbol, decimals, fxRate, tokenPriceUSD)
	}
	if err != nil {
		return nil, err
	}

	return &TokenQuote{
		Units:         tokenUnits,
		Amount:        FormatBaseUnits(tokenUnits, decimals),
		TokenPriceUSD: tokenPriceUSD,
		FXRate:        fxRate,
		FXCurrency:    currency,
		QuotedAt:      quotedAt,
	}, nil
}
