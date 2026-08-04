package solana

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/config"
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

// stablecoinPriceUSD returns the USD price for a stablecoin plus whether the $1
// peg holds. pegged=true means "use the peg verbatim" — the caller then converts
// with pure integer arithmetic, no rate involved. pegged=false is the depeg
// failsafe: a feed shows divergence beyond stablecoinPegTolerance, so the live
// price is returned (a $0.95 USDC yields a ~5% higher token charge so the
// merchant still nets the USD amount). Feedless stablecoins, a nil provider, or
// an unavailable/invalid feed all hold the peg.
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
// token with `decimals` base-unit precision. At the peg this is a pure integer
// rescale — micros * 10^(decimals-6) — and an exact CEILING divide when
// decimals < 6, so a fractional base unit never under-charges. No float touches
// this path: float64(micros)/1e6*1e6 is not the identity in IEEE-754 and
// overcharged 1.19% of whole-cent amounts by one base unit (#818 V2).
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

// fiatMicrosToBaseUnitsAtRate is the depeg/FX branch: base units =
// ceil(micros * fxRate * 10^decimals / (10^6 * tokenPriceUSD)), evaluated as an
// exact rational (rates converted to their exact rational value) with a single
// final ceiling — never a float multiply followed by math.Ceil.
func fiatMicrosToBaseUnitsAtRate(micros moneyutil.Micros, symbol string, decimals int, fxRate, tokenPriceUSD float64) (uint64, error) {
	if err := config.ValidateTokenDecimals(symbol, decimals); err != nil {
		return 0, err
	}
	if micros <= 0 {
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
	q := new(big.Rat).SetInt64(int64(micros))
	q.Mul(q, fxr)
	q.Mul(q, new(big.Rat).SetInt(pow10(decimals)))
	q.Quo(q, new(big.Rat).SetInt(pow10(microDecimals)))
	q.Quo(q, price)
	return baseUnitsToUint64(ceilRat(q), symbol)
}

// FiatMicrosToStablecoinBaseUnits converts a micro-USD amount into base units of
// a $1-pegged token with the merchant's configured `decimals` precision (#817 —
// hardcoding 6 undercharged a 9-decimal mint 1000x). At the peg the conversion
// is an exact integer rescale; the depeg failsafe rounds UP so a fractional base
// unit never under-charges the merchant.
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
	return fiatMicrosToBaseUnitsAtRate(micros, symbol, decimals, 1.0, priceUSD)
}

// FormatBaseUnits renders base units as a fixed-point decimal string with
// `decimals` fractional digits. Pure integer, no float rounding. This is the
// ONLY Solana amount formatter (#863 collapsed pay.go's trailing-zero-trimming
// twin into it): it reaches the Solana Pay `amount=` wire as well as display.
// Fixed precision is deliberate — it states the scale on the wire, so a wrong
// `decimals` makes the wallet reject the URL (decimal places > mint decimals)
// instead of silently transferring a 10^n-wrong amount.
func FormatBaseUnits(units uint64, decimals int) string {
	if decimals <= 0 {
		return strconv.FormatUint(units, 10)
	}
	div := pow10(decimals)
	whole, frac := new(big.Int).QuoRem(new(big.Int).SetUint64(units), div, new(big.Int))
	return fmt.Sprintf("%s.%0*s", whole.String(), decimals, frac.String())
}

// RequireTokenDecimals resolves the ctx merchant's configured base-unit
// precision for symbol from its armed Solana rail config. Fails closed: an
// unarmed rail, an unknown token, or a missing/zero decimals value are errors —
// guessing the scale is a 10^n mispricing.
func RequireTokenDecimals(ctx context.Context, src railresolve.Source, symbol string) (int, error) {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return 0, fmt.Errorf("solana: token symbol is required")
	}
	proc, err := RequireSolanaRailConfig(ctx, src)
	if err != nil {
		return 0, err
	}
	if proc.Solana == nil {
		return 0, fmt.Errorf("solana rail is not configured")
	}
	tok, ok := proc.Solana.Tokens[sym]
	if !ok {
		return 0, fmt.Errorf("solana: token %s is not configured for this merchant", sym)
	}
	if err := config.ValidateTokenDecimals(sym, tok.Decimals); err != nil {
		return 0, err
	}
	return tok.Decimals, nil
}

// RequireSolanaRailConfig resolves the ctx merchant's armed Solana rail
// account (Layer C, #788): the psps row's settings
// materialized into the runtime Solana config. Unarmed fails closed.
func RequireSolanaRailConfig(ctx context.Context, src railresolve.Source) (*config.PSPConfig, error) {
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
	return mint == "" || strings.EqualFold(mint, WrappedSOLMint)
}

// TokenQuote represents a complete quote for converting fiat to a Solana token.
// It includes all the information needed to audit and verify the quote.
// Units is the authoritative amount; Amount is its display rendering. Only the
// RATES are floats — every amount here is an integer.
type TokenQuote struct {
	Units           uint64           // base units to transfer (authoritative)
	Amount          string           // display only: Units at the token's decimals
	TokenPriceUSD   float64          // rate
	FXRate          float64          // rate
	FXCurrency      string           // the quoted price's currency
	AmountUSDMicros moneyutil.Micros // FX-converted price, ceilinged to whole micros
	QuotedAt        time.Time
}

type TokenPriceProvider interface {
	PriceUSD(ctx context.Context, symbol string) (float64, error)
}

// CalculateTokenQuote converts a fiat amount in MICROS (millionths of a major
// currency unit, any currency) into token units based on live prices.
func CalculateTokenQuote(ctx context.Context, tokenSymbol string, tokenCfg config.TokenConfig, amountMicros moneyutil.Micros, currency string, fxProvider fx.Provider, priceProvider TokenPriceProvider) (*TokenQuote, error) {
	tokenSymbol = strings.ToUpper(strings.TrimSpace(tokenSymbol))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("token symbol is required")
	}
	if err := config.ValidateTokenDecimals(tokenSymbol, tokenCfg.Decimals); err != nil {
		return nil, err
	}

	// #830: never invent a currency. The FX leg and the resulting on-chain charge
	// both key off it, so an absent code is a caller bug — not a "usd" default.
	// Validated against the money registry so an unregistered code cannot reach
	// the FX provider or the persisted quote.
	currency = strings.ToLower(strings.TrimSpace(currency))
	if currency == "" {
		return nil, fmt.Errorf("token quote requires a currency (refusing to default)")
	}
	if err := money.ValidateCurrency(currency); err != nil {
		return nil, err
	}
	if amountMicros <= 0 {
		return &TokenQuote{Units: 0, Amount: FormatBaseUnits(0, tokenCfg.Decimals), FXRate: 1.0, FXCurrency: currency, QuotedAt: time.Now()}, nil
	}

	quotedAt := time.Now()

	fxRate := 1.0
	if currency != "usd" {
		if fxProvider == nil {
			return nil, fmt.Errorf("FX conversion required for currency %s but no FX provider configured", currency)
		}
		fxQuote, err := fxProvider.QuoteToUSD(ctx, currency)
		if err != nil {
			return nil, fmt.Errorf("failed to get FX rate for %s: %w", currency, err)
		}
		fxRate = fxQuote.Rate
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
	atPeg := false
	if solanatokens.IsUSDPeggedToken(tokenSymbol, tokenCfg.Mint) {
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
	if atPeg && currency == "usd" {
		tokenUnits, err = FiatMicrosToBaseUnitsAtPeg(amountMicros, tokenSymbol, tokenCfg.Decimals)
	} else {
		tokenUnits, err = fiatMicrosToBaseUnitsAtRate(amountMicros, tokenSymbol, tokenCfg.Decimals, fxRate, tokenPriceUSD)
	}
	if err != nil {
		return nil, err
	}

	amountUSDMicros, err := microsAtRate(amountMicros, fxRate)
	if err != nil {
		return nil, err
	}

	return &TokenQuote{
		Units:           tokenUnits,
		Amount:          FormatBaseUnits(tokenUnits, tokenCfg.Decimals),
		TokenPriceUSD:   tokenPriceUSD,
		FXRate:          fxRate,
		FXCurrency:      currency,
		AmountUSDMicros: amountUSDMicros,
		QuotedAt:        quotedAt,
	}, nil
}

// microsAtRate applies an FX rate to a micros amount exactly, ceilinged to whole
// micros (audit value; the charge itself is computed from the unrounded rate).
func microsAtRate(micros moneyutil.Micros, rate float64) (moneyutil.Micros, error) {
	r, err := ratFromRate(rate, "fx rate")
	if err != nil {
		return 0, err
	}
	q := new(big.Rat).SetInt64(int64(micros))
	q.Mul(q, r)
	n := ceilRat(q)
	if !n.IsInt64() {
		return 0, fmt.Errorf("fx-converted amount %s overflows micros", n.String())
	}
	return moneyutil.Micros(n.Int64()), nil
}
