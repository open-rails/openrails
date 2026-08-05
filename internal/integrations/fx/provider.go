// Package fx provides foreign exchange rate conversion for billing operations.
package fx

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func normalizeCurrency(currency string) string {
	return money.NormalizeCurrency(currency)
}

// Quote represents an FX rate quote from a provider.
type Quote struct {
	// FromCurrency is the source currency (e.g., "EUR").
	FromCurrency string
	// ToCurrency is the target currency.
	ToCurrency string
	// Rate is the conversion rate (multiply source amount by this to get target amount).
	// For EUR->USD with rate 1.08, €10.00 * 1.08 = $10.80
	Rate float64
	// AsOf is when this rate was fetched/valid.
	AsOf time.Time
}

// Provider defines the interface for FX rate providers.
type Provider interface {
	// Quote returns the conversion rate from one supported currency to another.
	// For same-currency input, returns rate=1.0.
	Quote(ctx context.Context, fromCurrency, toCurrency string) (*Quote, error)
	// QuoteToUSD returns the conversion rate from the given currency to USD.
	// For USD input, returns rate=1.0.
	// Returns an error if the currency is not supported or the rate cannot be fetched.
	QuoteToUSD(ctx context.Context, currency string) (*Quote, error)
}

// ConvertAmount rounds a source amount up into the target currency's internal
// units. Enforcement paths use ceil so cross-currency spend cannot under-count
// because of fractional internal-unit conversion.
func ConvertAmount(ctx context.Context, provider Provider, fromCurrency, toCurrency string, amount int64) (int64, *Quote, error) {
	from := money.NormalizeCurrency(fromCurrency)
	to := money.NormalizeCurrency(toCurrency)
	if err := moneyutil.ValidateCurrency(from); err != nil {
		return 0, nil, err
	}
	if err := moneyutil.ValidateCurrency(to); err != nil {
		return 0, nil, err
	}
	if amount < 0 {
		return 0, nil, fmt.Errorf("amount must be non-negative")
	}
	quote := &Quote{FromCurrency: normalizeCurrency(from), ToCurrency: normalizeCurrency(to), Rate: 1, AsOf: time.Now()}
	if amount == 0 || from == to {
		return amount, quote, nil
	}
	if provider == nil {
		return 0, nil, fmt.Errorf("FX conversion required for %s -> %s but no FX provider configured", from, to)
	}
	q, err := provider.Quote(ctx, from, to)
	if err != nil {
		return 0, nil, err
	}
	if q == nil || q.Rate <= 0 {
		return 0, nil, fmt.Errorf("invalid FX quote for %s -> %s", from, to)
	}
	fromScale, ok := moneyutil.CurrencyScale(from)
	if !ok {
		return 0, nil, fmt.Errorf("money: unknown currency %q", from)
	}
	toScale, ok := moneyutil.CurrencyScale(to)
	if !ok {
		return 0, nil, fmt.Errorf("money: unknown currency %q", to)
	}
	rate, err := ratFromRate(q.Rate)
	if err != nil {
		return 0, nil, err
	}
	// MONEY-3: the RATE is a float, the AMOUNT never is. amount * rate *
	// 10^(toScale-fromScale) is evaluated as an exact rational with a single
	// final ceiling — a float multiply followed by math.Ceil rounds the binary
	// representation error up into a phantom internal unit.
	v := new(big.Rat).SetInt64(amount)
	v.Mul(v, rate)
	if shift := toScale - fromScale; shift >= 0 {
		v.Mul(v, new(big.Rat).SetInt(pow10(shift)))
	} else {
		v.Quo(v, new(big.Rat).SetInt(pow10(-shift)))
	}
	n := ceilRat(v)
	if !n.IsInt64() {
		return 0, nil, fmt.Errorf("converted amount overflows int64")
	}
	return n.Int64(), q, nil
}

// ratFromRate converts an FX RATE (legitimately a float) to the exact rational
// of its shortest DECIMAL form — 1.08 becomes 108/100, not the binary double
// 1.0800000000000000710…. Mirrors internal/modules/solana's helper of the same
// name. Past this point no amount touches a float.
func ratFromRate(rate float64) (*big.Rat, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0 {
		return nil, fmt.Errorf("invalid fx rate %v", rate)
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(rate, 'g', -1, 64))
	if !ok || r.Sign() <= 0 {
		return nil, fmt.Errorf("invalid fx rate %v", rate)
	}
	return r, nil
}

func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// ceilRat returns ceil(r) for r >= 0.
func ceilRat(r *big.Rat) *big.Int {
	q, rem := new(big.Int).QuoRem(r.Num(), r.Denom(), new(big.Int))
	if rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q
}
