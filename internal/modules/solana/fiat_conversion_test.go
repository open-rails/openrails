package solana

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestFiatMicrosToStablecoinBaseUnits pins the micro-USD -> stablecoin base-unit
// conversion at the merchant's CONFIGURED decimals (#817) plus the depeg failsafe.
func TestFiatMicrosToStablecoinBaseUnits(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		micros   moneyutil.Micros
		decimals int
		symbol   string
		provider TokenPriceProvider
		want     uint64
	}{
		{name: "one dollar at 6 decimals", micros: 1_000_000, decimals: 6, want: 1_000_000},
		{name: "sub-cent micros at 6 decimals", micros: 1, decimals: 6, want: 1},
		{name: "nineteen ninety nine at 6 decimals", micros: 19_990_000, decimals: 6, want: 19_990_000},
		{name: "zero micros", micros: 0, decimals: 6, want: 0},
		{name: "negative micros clamp", micros: -500, decimals: 6, want: 0},

		// #817: a 9-decimal mint needs 1000x the base units of a 6-decimal one.
		{name: "ten dollars at 9 decimals", micros: 10_000_000, decimals: 9, want: 10_000_000_000},
		{name: "one dollar at 9 decimals", micros: 1_000_000, decimals: 9, want: 1_000_000_000},
		// 8 decimals: 100x.
		{name: "ten dollars at 8 decimals", micros: 10_000_000, decimals: 8, want: 1_000_000_000},
		{name: "nineteen ninety nine at 8 decimals", micros: 19_990_000, decimals: 8, want: 1_999_000_000},
		// Below micro precision: exact divide, rounded UP (never under-charge).
		{name: "two decimals exact", micros: 10_000_000, decimals: 2, want: 1_000},
		{name: "two decimals rounds up", micros: 10_000_001, decimals: 2, want: 1_001},

		{
			name: "minor depeg within tolerance ignored", micros: 1_000_000, decimals: 6,
			symbol: "USDC", provider: fakeTokenPriceProvider{"USDC": 0.995}, want: 1_000_000,
		},
		{
			name: "depeg failsafe scales up", micros: 1_000_000, decimals: 6,
			symbol: "USDC", provider: fakeTokenPriceProvider{"USDC": 0.95}, want: 1_052_632,
		},
		{
			name: "depeg failsafe honours decimals", micros: 1_000_000, decimals: 9,
			symbol: "USDC", provider: fakeTokenPriceProvider{"USDC": 0.95}, want: 1_052_631_579,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			symbol := tt.symbol
			if symbol == "" {
				symbol = "USDC"
			}
			got, err := FiatMicrosToStablecoinBaseUnits(ctx, tt.micros, symbol, tt.decimals, tt.provider)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %d base units, got %d", tt.want, got)
			}
		})
	}
}

// TestFiatMicrosRejectsMissingDecimals pins #817: an OMITTED `decimals` decodes
// as 0 and must fail loudly instead of mispricing by 10^6.
func TestFiatMicrosRejectsMissingDecimals(t *testing.T) {
	for _, d := range []int{0, -1, 19} {
		if _, err := FiatMicrosToStablecoinBaseUnits(context.Background(), 1_000_000, "USDC", d, nil); err == nil {
			t.Fatalf("decimals=%d: expected an error", d)
		}
		if _, err := FiatMicrosToBaseUnitsAtPeg(1_000_000, "USDC", d); err == nil {
			t.Fatalf("decimals=%d (at peg): expected an error", d)
		}
	}
}

// TestPegConversionHasNoFloatOffByOne is the #818 V2 regression: the old float
// chain — (float64(micros)/1e6) * math.Pow10(decimals) fed to math.Ceil —
// overcharged 1.19% of whole-cent amounts by exactly one base unit. These are
// the first failures found by exhaustive scan. The peg conversion is now an
// exact integer rescale, so every amount must be micros*10^(d-6) EXACTLY.
func TestPegConversionHasNoFloatOffByOne(t *testing.T) {
	knownBad := []moneyutil.Micros{4_030_000, 4_070_000, 4_110_000, 4_150_000, 4_190_000, 8_050_000, 8_060_000, 8_130_000}
	for _, micros := range knownBad {
		for _, d := range []int{6, 8, 9} {
			got, err := FiatMicrosToBaseUnitsAtPeg(micros, "USDC", d)
			if err != nil {
				t.Fatalf("%d micros @%d: %v", micros, d, err)
			}
			want := uint64(micros) * pow10(d-6).Uint64()
			if got != want {
				t.Fatalf("%d micros @%d decimals: got %d base units, want %d (off by %d)", micros, d, got, want, int64(got)-int64(want))
			}
		}
	}
}

// TestPegConversionExhaustiveWholeCents sweeps the whole-cent range that the
// float chain corrupted (2,382 of the first 200,000 amounts) and asserts an
// exact rescale for every one.
func TestPegConversionExhaustiveWholeCents(t *testing.T) {
	for cents := int64(1); cents <= 200_000; cents++ {
		micros := moneyutil.Micros(cents * 10_000)
		got, err := FiatMicrosToBaseUnitsAtPeg(micros, "USDC", 6)
		if err != nil {
			t.Fatalf("%d micros: %v", micros, err)
		}
		if got != uint64(micros) {
			t.Fatalf("%d micros: got %d base units, want %d", micros, got, uint64(micros))
		}
	}
}

func TestFormatBaseUnits(t *testing.T) {
	cases := []struct {
		units    uint64
		decimals int
		want     string
	}{
		{10_000_000, 6, "10.000000"},
		{19_990_000, 6, "19.990000"},
		{1, 6, "0.000001"},
		{10_000_000_000, 9, "10.000000000"},
		{0, 6, "0.000000"},
	}
	for _, c := range cases {
		if got := FormatBaseUnits(c.units, c.decimals); got != c.want {
			t.Fatalf("FormatBaseUnits(%d, %d) = %q, want %q", c.units, c.decimals, got, c.want)
		}
	}
}
