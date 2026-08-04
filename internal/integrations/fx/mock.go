package fx

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/modules/money"
)

// MockProvider is a test double for FX rate provider.
type MockProvider struct {
	// Rates maps currency codes to USD rates.
	// e.g., {"eur": 1.08, "gbp": 1.27}
	Rates map[string]float64

	// Error, if set, will be returned for all calls.
	Error error

	// CallCount tracks how many times Quote was called.
	CallCount int

	// LastCurrency records the last source currency requested.
	LastCurrency string
	// LastToCurrency records the last target currency requested.
	LastToCurrency string
}

// NewMockProvider creates a MockProvider with the given rates.
func NewMockProvider(rates map[string]float64) *MockProvider {
	normalized := make(map[string]float64, len(rates)+1)
	for code, rate := range rates {
		normalized[normalizeCurrency(code)] = rate
	}
	// Always include USD
	normalized[money.DefaultCurrency] = 1.0
	return &MockProvider{
		Rates: normalized,
	}
}

// Quote returns a mock quote based on configured rates to USD, composing pairs
// through USD without making USD a caller-visible default.
func (p *MockProvider) Quote(ctx context.Context, fromCurrency, toCurrency string) (*Quote, error) {
	p.CallCount++
	fromCurrency = normalizeCurrency(fromCurrency)
	toCurrency = normalizeCurrency(toCurrency)
	p.LastCurrency = fromCurrency
	p.LastToCurrency = toCurrency

	if p.Error != nil {
		return nil, p.Error
	}

	fromUSD, ok := p.Rates[fromCurrency]
	if !ok {
		return nil, fmt.Errorf("unsupported currency: %s", fromCurrency)
	}
	toUSD, ok := p.Rates[toCurrency]
	if !ok {
		return nil, fmt.Errorf("unsupported currency: %s", toCurrency)
	}
	if toUSD <= 0 {
		return nil, fmt.Errorf("invalid currency rate: %s", toCurrency)
	}

	return &Quote{
		FromCurrency: fromCurrency,
		ToCurrency:   toCurrency,
		Rate:         fromUSD / toUSD,
		AsOf:         time.Now(),
	}, nil
}

// QuoteToUSD returns a mock quote to USD.
func (p *MockProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	return p.Quote(ctx, currency, money.DefaultCurrency)
}

// SetRate sets the rate for a specific currency.
func (p *MockProvider) SetRate(currency string, rate float64) {
	p.Rates[normalizeCurrency(currency)] = rate
}

// Reset clears call tracking.
func (p *MockProvider) Reset() {
	p.CallCount = 0
	p.LastCurrency = ""
	p.LastToCurrency = ""
	p.Error = nil
}
