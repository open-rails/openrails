package payments

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/fx"
)

func TestCalculateTokenQuote_USDPrice(t *testing.T) {
	// Skip this test if it requires real Jupiter API
	// In a real implementation, we'd inject a mock Jupiter client
	t.Skip("Requires mock Jupiter client - integration test only")

	tokenCfg := config.SolanaToken{
		Symbol:   "USDC",
		Mint:     "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		Decimals: 6,
		Enabled:  true,
	}

	quote, err := CalculateTokenQuote(context.Background(), tokenCfg, 1000, "usd", nil)
	if err != nil {
		t.Fatalf("CalculateTokenQuote() error: %v", err)
	}

	// For $10.00 USD with USDC at $1.00, we expect ~10 USDC
	if quote.FXRate != 1.0 {
		t.Errorf("FXRate = %v, want 1.0", quote.FXRate)
	}
	if quote.FXCurrency != "usd" {
		t.Errorf("FXCurrency = %v, want usd", quote.FXCurrency)
	}
}

func TestCalculateTokenQuote_NonUSDPrice_RequiresFXProvider(t *testing.T) {
	tokenCfg := config.SolanaToken{
		Symbol:   "USDC",
		Mint:     "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		Decimals: 6,
		Enabled:  true,
	}

	// Without FX provider, non-USD should fail
	_, err := CalculateTokenQuote(context.Background(), tokenCfg, 1000, "eur", nil)
	if err == nil {
		t.Error("CalculateTokenQuote() expected error for EUR without FX provider, got nil")
	}
}

func TestCalculateTokenQuote_ZeroAmount(t *testing.T) {
	tokenCfg := config.SolanaToken{
		Symbol:   "USDC",
		Mint:     "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		Decimals: 6,
		Enabled:  true,
	}

	quote, err := CalculateTokenQuote(context.Background(), tokenCfg, 0, "usd", nil)
	if err != nil {
		t.Fatalf("CalculateTokenQuote() error: %v", err)
	}

	if quote.Units != 0 {
		t.Errorf("Units = %v, want 0", quote.Units)
	}
	if quote.Decimal != 0 {
		t.Errorf("Decimal = %v, want 0", quote.Decimal)
	}
}

func TestCalculateTokenQuote_EmptyCurrencyDefaultsToUSD(t *testing.T) {
	// Skip - requires Jupiter API
	t.Skip("Requires mock Jupiter client - integration test only")

	tokenCfg := config.SolanaToken{
		Symbol:   "USDC",
		Mint:     "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		Decimals: 6,
		Enabled:  true,
	}

	quote, err := CalculateTokenQuote(context.Background(), tokenCfg, 1000, "", nil)
	if err != nil {
		t.Fatalf("CalculateTokenQuote() error: %v", err)
	}

	if quote.FXCurrency != "usd" {
		t.Errorf("FXCurrency = %v, want usd (default)", quote.FXCurrency)
	}
}

func TestCalculateTokenQuote_MissingMint(t *testing.T) {
	tokenCfg := config.SolanaToken{
		Symbol:   "TEST",
		Decimals: 6,
		Enabled:  true,
		// No Mint set
	}

	_, err := CalculateTokenQuote(context.Background(), tokenCfg, 1000, "usd", nil)
	if err == nil {
		t.Error("CalculateTokenQuote() expected error for missing mint, got nil")
	}
}

func TestCalculateTokenQuote_WithMockFXProvider(t *testing.T) {
	// Skip - requires Jupiter API for token prices
	t.Skip("Requires mock Jupiter client - integration test only")

	tokenCfg := config.SolanaToken{
		Symbol:   "USDC",
		Mint:     "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		Decimals: 6,
		Enabled:  true,
	}

	mockFX := fx.NewMockProvider(map[string]float64{
		"eur": 1.08, // 1 EUR = 1.08 USD
		"gbp": 1.27, // 1 GBP = 1.27 USD
	})

	// Test EUR conversion
	quote, err := CalculateTokenQuote(context.Background(), tokenCfg, 1000, "eur", mockFX)
	if err != nil {
		t.Fatalf("CalculateTokenQuote() error: %v", err)
	}

	// €10.00 * 1.08 = $10.80 USD
	expectedAmountUSD := 10.80
	if quote.AmountUSD != expectedAmountUSD {
		t.Errorf("AmountUSD = %v, want %v", quote.AmountUSD, expectedAmountUSD)
	}
	if quote.FXRate != 1.08 {
		t.Errorf("FXRate = %v, want 1.08", quote.FXRate)
	}
	if quote.FXCurrency != "eur" {
		t.Errorf("FXCurrency = %v, want eur", quote.FXCurrency)
	}
}

func TestTokenQuoteStruct(t *testing.T) {
	// Test that TokenQuote struct has all required fields
	quote := &TokenQuote{
		Units:         1000000,
		Decimal:       1.0,
		TokenPriceUSD: 1.0,
		FXRate:        1.08,
		FXCurrency:    "eur",
		AmountUSD:     10.80,
	}

	if quote.Units != 1000000 {
		t.Errorf("Units = %v, want 1000000", quote.Units)
	}
	if quote.Decimal != 1.0 {
		t.Errorf("Decimal = %v, want 1.0", quote.Decimal)
	}
	if quote.TokenPriceUSD != 1.0 {
		t.Errorf("TokenPriceUSD = %v, want 1.0", quote.TokenPriceUSD)
	}
	if quote.FXRate != 1.08 {
		t.Errorf("FXRate = %v, want 1.08", quote.FXRate)
	}
	if quote.FXCurrency != "eur" {
		t.Errorf("FXCurrency = %v, want eur", quote.FXCurrency)
	}
	if quote.AmountUSD != 10.80 {
		t.Errorf("AmountUSD = %v, want 10.80", quote.AmountUSD)
	}
}

func TestNormalizeStablecoinPrice(t *testing.T) {
	tests := []struct {
		name     string
		symbol   string
		price    float64
		expected float64
	}{
		// USDC within tolerance - should round to 1.0
		{"USDC at 0.9998", "USDC", 0.9998, 1.0},
		{"USDC at 1.0001", "USDC", 1.0001, 1.0},
		{"USDC at 0.99", "USDC", 0.99, 1.0},
		{"USDC at 1.01", "USDC", 1.01, 1.0},
		{"USDC at 0.98 (edge)", "USDC", 0.98, 1.0},
		{"USDC at 1.02 (edge)", "USDC", 1.02, 1.0},

		// USDC outside tolerance - should use actual price (depeg scenario)
		{"USDC at 0.95 (depeg)", "USDC", 0.95, 0.95},
		{"USDC at 0.97 (depeg)", "USDC", 0.97, 0.97},
		{"USDC at 1.05 (premium)", "USDC", 1.05, 1.05},

		// Other stablecoins
		{"USDT at 0.999", "USDT", 0.999, 1.0},
		{"DAI at 1.001", "DAI", 1.001, 1.0},
		{"PYUSD at 0.9999", "PYUSD", 0.9999, 1.0},

		// Non-stablecoins should not be affected
		{"SOL unchanged", "SOL", 95.50, 95.50},
		{"BONK unchanged", "BONK", 0.00001234, 0.00001234},
		{"ETH unchanged", "ETH", 2500.00, 2500.00},

		// Case insensitivity
		{"usdc lowercase", "usdc", 0.9998, 1.0},
		{"Usdc mixed case", "Usdc", 0.9998, 1.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := normalizeStablecoinPrice(tt.symbol, tt.price)
			if result != tt.expected {
				t.Errorf("normalizeStablecoinPrice(%q, %v) = %v, want %v",
					tt.symbol, tt.price, result, tt.expected)
			}
		})
	}
}
