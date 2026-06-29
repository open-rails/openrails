package fx

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A canceled context must surface as context.Canceled all the way up through the
// %w wrapping, because RedisCachedProvider.Start relies on
// errors.Is(err, context.Canceled) to suppress shutdown noise. If any link in
// the chain ever switches %w -> %v this fails, catching the regression before it
// logs "fx: refresh failed" on every clean shutdown again.
func TestExchangeAPIProvider_CanceledContextIsDetectable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewExchangeAPIProvider().Quote(ctx, "eur", "usd")
	if err == nil {
		t.Fatal("want error from canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("errors.Is(err, context.Canceled) must be true (the shutdown-noise guard depends on it); got %v", err)
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
