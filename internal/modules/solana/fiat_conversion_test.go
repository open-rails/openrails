package solana

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestFiatMicrosToStablecoinBaseUnits pins the native micro-USD -> stablecoin
// base-unit conversion at the $1 peg plus the depeg failsafe.
func TestFiatMicrosToStablecoinBaseUnits(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		micros   moneyutil.Micros
		symbol   string
		provider TokenPriceProvider
		want     uint64
	}{
		{
			name:   "one dollar at peg",
			micros: 1_000_000,
			want:   1_000_000,
		},
		{
			name:   "sub-cent micros at peg",
			micros: 1,
			want:   1,
		},
		{
			name:   "nineteen ninety nine at peg",
			micros: 19_990_000,
			want:   19_990_000,
		},
		{
			name:   "zero micros",
			micros: 0,
			want:   0,
		},
		{
			name:   "negative micros clamp",
			micros: -500,
			want:   0,
		},
		{
			name:     "minor depeg within tolerance ignored",
			micros:   1_000_000,
			symbol:   "USDC",
			provider: fakeTokenPriceProvider{"USDC": 0.995},
			want:     1_000_000,
		},
		{
			name:     "depeg failsafe scales up",
			micros:   1_000_000,
			symbol:   "USDC",
			provider: fakeTokenPriceProvider{"USDC": 0.95},
			want:     1_052_632,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symbol := tt.symbol
			if symbol == "" {
				symbol = "USDC"
			}
			got := FiatMicrosToStablecoinBaseUnits(ctx, tt.micros, symbol, tt.provider)
			if got != tt.want {
				t.Fatalf("expected %d base units, got %d", tt.want, got)
			}
		})
	}
}
