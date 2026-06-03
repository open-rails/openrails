package solana

import (
	"context"
	"testing"
)

// TestFiatCentsToStablecoinBaseUnits pins the #267 cents -> stablecoin base-unit
// conversion at the $1 peg plus the depeg failsafe.
func TestFiatCentsToStablecoinBaseUnits(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		cents    int64
		symbol   string
		provider TokenPriceProvider
		want     uint64
	}{
		{
			// $1.00 = 100 cents -> 1 USDC -> 1_000_000 base units (6 decimals).
			name:  "one dollar at peg",
			cents: 100,
			want:  1_000_000,
		},
		{
			// $50.00 = 5000 cents -> 50 USDC -> 50_000_000 base units.
			name:  "fifty dollars at peg",
			cents: 5000,
			want:  50_000_000,
		},
		{
			// Issue example reduced first charge $31.34 = 3134 cents.
			name:  "prorated upgrade first charge",
			cents: 3134,
			want:  31_340_000,
		},
		{
			// One cent -> 10_000 base units exactly.
			name:  "one cent",
			cents: 1,
			want:  10_000,
		},
		{
			// Zero / negative clamps to 0.
			name:  "zero cents",
			cents: 0,
			want:  0,
		},
		{
			name:  "negative cents clamp",
			cents: -500,
			want:  0,
		},
		{
			// Within the 1% peg tolerance => still treated as $1.00.
			name:     "minor depeg within tolerance ignored",
			cents:    100,
			symbol:   "USDC",
			provider: fakeTokenPriceProvider{"USDC": 0.995},
			want:     1_000_000,
		},
		{
			// Real depeg beyond tolerance (USDC at $0.95): need MORE tokens so the
			// merchant still nets the USD. $1 / 0.95 = 1.0526... USDC, ceil to base
			// units => 1_052_632.
			name:     "depeg failsafe scales up",
			cents:    100,
			symbol:   "USDC",
			provider: fakeTokenPriceProvider{"USDC": 0.95},
			want:     1_052_632,
		},
		{
			// Nil provider falls back to the $1.00 peg.
			name:  "nil provider uses peg",
			cents: 100,
			want:  1_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symbol := tt.symbol
			if symbol == "" {
				symbol = "USDC"
			}
			got := FiatCentsToStablecoinBaseUnits(ctx, tt.cents, symbol, tt.provider)
			if got != tt.want {
				t.Fatalf("expected %d base units, got %d", tt.want, got)
			}
		})
	}
}
