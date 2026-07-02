// Package fx provides foreign exchange rate conversion for billing operations.
package fx

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
)

func normalizeCurrency(currency string) string {
	return strings.ToLower(strings.TrimSpace(currency))
}

// Quote represents an FX rate quote from a provider.
type Quote struct {
	// FromCurrency is the source currency (e.g., "eur").
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
	if err := money.ValidateCurrency(from); err != nil {
		return 0, nil, err
	}
	if err := money.ValidateCurrency(to); err != nil {
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
	fromScale, ok := money.CurrencyScale(from)
	if !ok {
		return 0, nil, fmt.Errorf("money: unknown currency %q", from)
	}
	toScale, ok := money.CurrencyScale(to)
	if !ok {
		return 0, nil, fmt.Errorf("money: unknown currency %q", to)
	}
	converted := float64(amount) * q.Rate * math.Pow10(toScale-fromScale)
	if converted > float64(math.MaxInt64) {
		return 0, nil, fmt.Errorf("converted amount overflows int64")
	}
	return int64(math.Ceil(converted)), q, nil
}
