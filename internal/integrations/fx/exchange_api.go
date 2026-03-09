package fx

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const (
	// exchangeAPIBaseURL is the base URL for the CC0 exchange-api.
	// Source: https://github.com/fawazahmed0/exchange-api
	// No API key required, no attribution required (CC0 license).
	exchangeAPIBaseURL = "https://latest.currency-api.pages.dev/v1/currencies"

	// fallbackBaseURL is the fallback URL if the primary is unavailable.
	fallbackBaseURL = "https://cdn.jsdelivr.net/npm/@fawazahmed0/currency-api@latest/v1/currencies"
)

// ExchangeAPIProvider fetches FX rates from the CC0 exchange-api.
type ExchangeAPIProvider struct {
	client  *http.Client
	baseURL string
}

// NewExchangeAPIProvider creates a new ExchangeAPIProvider with default settings.
func NewExchangeAPIProvider() *ExchangeAPIProvider {
	return &ExchangeAPIProvider{
		client: &http.Client{
			Timeout: 5 * time.Second,
		},
		baseURL: exchangeAPIBaseURL,
	}
}

// QuoteToUSD fetches the conversion rate from the given currency to USD.
func (p *ExchangeAPIProvider) QuoteToUSD(ctx context.Context, currency string) (*Quote, error) {
	currency = strings.ToLower(strings.TrimSpace(currency))

	// Short-circuit for USD
	if currency == "usd" {
		return &Quote{
			FromCurrency: "usd",
			ToCurrency:   "usd",
			Rate:         1.0,
			AsOf:         time.Now(),
		}, nil
	}

	// Try primary URL first, then fallback
	rate, asOf, err := p.fetchRate(ctx, p.baseURL, currency)
	if err != nil {
		// Try fallback
		rate, asOf, err = p.fetchRate(ctx, fallbackBaseURL, currency)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch FX rate for %s: %w", currency, err)
		}
	}

	return &Quote{
		FromCurrency: currency,
		ToCurrency:   "usd",
		Rate:         rate,
		AsOf:         asOf,
	}, nil
}

func (p *ExchangeAPIProvider) fetchRate(ctx context.Context, baseURL, currency string) (float64, time.Time, error) {
	url := fmt.Sprintf("%s/%s.json", baseURL, currency)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, time.Time{}, err
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, time.Time{}, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	// Parse the response - it's a dynamic structure where the currency code is a key
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return 0, time.Time{}, fmt.Errorf("failed to decode response: %w", err)
	}

	// Parse the date
	var dateStr string
	if dateRaw, ok := raw["date"]; ok {
		if err := json.Unmarshal(dateRaw, &dateStr); err != nil {
			return 0, time.Time{}, fmt.Errorf("failed to parse date: %w", err)
		}
	}

	asOf := time.Now()
	if dateStr != "" {
		if parsed, err := timeutil.ParseDateUTC(dateStr); err == nil {
			asOf = parsed
		}
	}

	// Parse the rates for the requested currency
	ratesRaw, ok := raw[currency]
	if !ok {
		return 0, time.Time{}, fmt.Errorf("currency %s not found in response", currency)
	}

	var rates map[string]float64
	if err := json.Unmarshal(ratesRaw, &rates); err != nil {
		return 0, time.Time{}, fmt.Errorf("failed to parse rates: %w", err)
	}

	usdRate, ok := rates["usd"]
	if !ok {
		return 0, time.Time{}, fmt.Errorf("USD rate not found for currency %s", currency)
	}

	if usdRate <= 0 {
		return 0, time.Time{}, fmt.Errorf("invalid USD rate for currency %s: %f", currency, usdRate)
	}

	return usdRate, asOf, nil
}
