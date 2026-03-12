// Package fx provides foreign exchange rate conversion for billing operations.
package fx

import (
	"context"
	"strings"
	"time"
)

func normalizeCurrency(currency string) string {
	return strings.ToLower(strings.TrimSpace(currency))
}

// Quote represents an FX rate quote from a provider.
type Quote struct {
	// FromCurrency is the source currency (e.g., "eur").
	FromCurrency string
	// ToCurrency is the target currency (always "usd" for our use case).
	ToCurrency string
	// Rate is the conversion rate (multiply source amount by this to get target amount).
	// For EUR->USD with rate 1.08, €10.00 * 1.08 = $10.80
	Rate float64
	// AsOf is when this rate was fetched/valid.
	AsOf time.Time
}

// Provider defines the interface for FX rate providers.
type Provider interface {
	// QuoteToUSD returns the conversion rate from the given currency to USD.
	// For USD input, returns rate=1.0.
	// Returns an error if the currency is not supported or the rate cannot be fetched.
	QuoteToUSD(ctx context.Context, currency string) (*Quote, error)
}

// NoOpProvider returns rate=1.0 for all currencies (useful for testing or USD-only deployments).
type NoOpProvider struct{}

// QuoteToUSD always returns rate=1.0 for NoOpProvider.
func (p *NoOpProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	currency = normalizeCurrency(currency)
	return &Quote{
		FromCurrency: currency,
		ToCurrency:   "usd",
		Rate:         1.0,
		AsOf:         time.Now(),
	}, nil
}
