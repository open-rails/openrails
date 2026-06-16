package fx

import (
	"context"
	"testing"
	"time"
)

func TestMockProvider_QuoteToUSD(t *testing.T) {
	tests := []struct {
		name        string
		rates       map[string]float64
		currency    string
		wantRate    float64
		wantErr     bool
		providerErr error
	}{
		{
			name:     "USD returns 1.0",
			rates:    map[string]float64{},
			currency: "usd",
			wantRate: 1.0,
			wantErr:  false,
		},
		{
			name:     "EUR conversion",
			rates:    map[string]float64{"eur": 1.08},
			currency: "eur",
			wantRate: 1.08,
			wantErr:  false,
		},
		{
			name:     "GBP conversion",
			rates:    map[string]float64{"gbp": 1.27},
			currency: "gbp",
			wantRate: 1.27,
			wantErr:  false,
		},
		{
			name:     "unsupported currency",
			rates:    map[string]float64{"eur": 1.08},
			currency: "xyz",
			wantRate: 0,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewMockProvider(tt.rates)
			if tt.providerErr != nil {
				p.Error = tt.providerErr
			}

			quote, err := p.QuoteToUSD(context.Background(), tt.currency)

			if tt.wantErr {
				if err == nil {
					t.Errorf("QuoteToUSD() expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Errorf("QuoteToUSD() unexpected error: %v", err)
				return
			}

			if quote.Rate != tt.wantRate {
				t.Errorf("QuoteToUSD() rate = %v, want %v", quote.Rate, tt.wantRate)
			}

			if quote.FromCurrency != tt.currency {
				t.Errorf("QuoteToUSD() from = %v, want %v", quote.FromCurrency, tt.currency)
			}

			if quote.ToCurrency != "usd" {
				t.Errorf("QuoteToUSD() to = %v, want usd", quote.ToCurrency)
			}
		})
	}
}

func TestMockProvider_QuotePair_ComposesViaUSD(t *testing.T) {
	p := NewMockProvider(map[string]float64{"eur": 1.08, "jpy": 0.0067})

	quote, err := p.Quote(context.Background(), "eur", "jpy")
	if err != nil {
		t.Fatalf("Quote() unexpected error: %v", err)
	}
	want := 1.08 / 0.0067
	if quote.Rate != want {
		t.Fatalf("Quote() rate = %v, want %v", quote.Rate, want)
	}
	if quote.FromCurrency != "eur" || quote.ToCurrency != "jpy" {
		t.Fatalf("Quote() pair = %s -> %s, want eur -> jpy", quote.FromCurrency, quote.ToCurrency)
	}
}

func TestConvertAmount_RoundsUpAcrossCurrencyScales(t *testing.T) {
	p := NewMockProvider(map[string]float64{"eur": 1.25, "jpy": 0.01})

	tests := []struct {
		name string
		from string
		to   string
		amt  int64
		want int64
	}{
		{name: "usd to eur same scale", from: "USD", to: "EUR", amt: 1_000_000, want: 800_000},
		{name: "usd to jpy lower scale rounds up", from: "USD", to: "JPY", amt: 123_456, want: 123_456},
		{name: "jpy to usd higher scale", from: "JPY", to: "USD", amt: 123, want: 123},
		{name: "same currency no provider needed", from: "USD", to: "USD", amt: 42, want: 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := ConvertAmount(context.Background(), p, tt.from, tt.to, tt.amt)
			if err != nil {
				t.Fatalf("ConvertAmount() unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("ConvertAmount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestConvertAmount_CrossCurrencyRequiresProvider(t *testing.T) {
	if _, _, err := ConvertAmount(context.Background(), nil, "USD", "EUR", 1); err == nil {
		t.Fatal("ConvertAmount() expected missing provider error")
	}
	got, _, err := ConvertAmount(context.Background(), nil, "USD", "USD", 1)
	if err != nil {
		t.Fatalf("ConvertAmount() same-currency unexpected error: %v", err)
	}
	if got != 1 {
		t.Fatalf("ConvertAmount() same-currency = %d, want 1", got)
	}
}

func TestMockProvider_CallTracking(t *testing.T) {
	p := NewMockProvider(map[string]float64{"eur": 1.08})

	// Initial state
	if p.CallCount != 0 {
		t.Errorf("Initial CallCount = %d, want 0", p.CallCount)
	}

	// First call
	_, _ = p.QuoteToUSD(context.Background(), "eur")
	if p.CallCount != 1 {
		t.Errorf("After first call, CallCount = %d, want 1", p.CallCount)
	}
	if p.LastCurrency != "eur" {
		t.Errorf("LastCurrency = %s, want eur", p.LastCurrency)
	}

	// Second call
	_, _ = p.QuoteToUSD(context.Background(), "usd")
	if p.CallCount != 2 {
		t.Errorf("After second call, CallCount = %d, want 2", p.CallCount)
	}
	if p.LastCurrency != "usd" {
		t.Errorf("LastCurrency = %s, want usd", p.LastCurrency)
	}

	// Reset
	p.Reset()
	if p.CallCount != 0 {
		t.Errorf("After Reset, CallCount = %d, want 0", p.CallCount)
	}
}

func TestCachedProvider_CachesResults(t *testing.T) {
	mock := NewMockProvider(map[string]float64{"eur": 1.08})
	cached := NewCachedProvider(mock, 5*time.Minute)

	// First call - should hit the provider
	quote1, err := cached.QuoteToUSD(context.Background(), "eur")
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}
	if mock.CallCount != 1 {
		t.Errorf("First call: provider CallCount = %d, want 1", mock.CallCount)
	}
	if quote1.Rate != 1.08 {
		t.Errorf("First call: rate = %v, want 1.08", quote1.Rate)
	}

	// Second call - should use cache
	quote2, err := cached.QuoteToUSD(context.Background(), "eur")
	if err != nil {
		t.Fatalf("Second call failed: %v", err)
	}
	if mock.CallCount != 1 {
		t.Errorf("Second call: provider CallCount = %d, want 1 (cached)", mock.CallCount)
	}
	if quote2.Rate != 1.08 {
		t.Errorf("Second call: rate = %v, want 1.08", quote2.Rate)
	}

	// Different currency - should hit provider
	_, _ = cached.QuoteToUSD(context.Background(), "gbp")
	// This will fail since GBP isn't in mock, but that's expected
}

func TestCachedProvider_InvalidateAll(t *testing.T) {
	mock := NewMockProvider(map[string]float64{"eur": 1.08})
	cached := NewCachedProvider(mock, 5*time.Minute)

	// Populate cache
	_, _ = cached.QuoteToUSD(context.Background(), "eur")
	if mock.CallCount != 1 {
		t.Errorf("Initial call: CallCount = %d, want 1", mock.CallCount)
	}

	// Invalidate
	cached.InvalidateAll()

	// Should hit provider again
	_, _ = cached.QuoteToUSD(context.Background(), "eur")
	if mock.CallCount != 2 {
		t.Errorf("After invalidate: CallCount = %d, want 2", mock.CallCount)
	}
}

func TestNoOpProvider_AlwaysReturnsOne(t *testing.T) {
	p := &NoOpProvider{}

	currencies := []string{"usd", "eur", "gbp", "jpy", "xyz"}
	for _, currency := range currencies {
		quote, err := p.QuoteToUSD(context.Background(), currency)
		if err != nil {
			t.Errorf("QuoteToUSD(%s) unexpected error: %v", currency, err)
			continue
		}
		if quote.Rate != 1.0 {
			t.Errorf("QuoteToUSD(%s) rate = %v, want 1.0", currency, quote.Rate)
		}
	}
}
