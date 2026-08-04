package solana

import (
	"context"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestFormatBaseUnitsWirePins is the wire pin for the ONE Solana amount
// formatter (#863: pay.go's untested `formatTokenAmount` twin was deleted in
// favour of this one). Known base units + decimals => the exact string that
// goes out as Solana Pay's `amount=`. Fixed precision, no trailing-zero
// trimming, no rounding — the split is exact integer QuoRem.
func TestFormatBaseUnitsWirePins(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		units    uint64
		decimals int
		want     string
	}{
		// 6 decimals — USDC/USDT, what every live merchant sends today.
		{"usdc one", 1_000_000, 6, "1.000000"},
		{"usdc ten", 10_000_000, 6, "10.000000"},
		{"usdc nineteen ninety nine", 19_990_000, 6, "19.990000"},
		{"usdc five", 5_000_000, 6, "5.000000"},
		{"usdc one base unit", 1, 6, "0.000001"},
		{"usdc zero", 0, 6, "0.000000"},
		// Whole/fraction boundary: one base unit below and at the unit.
		{"usdc just under one", 999_999, 6, "0.999999"},
		{"usdc just over one", 1_000_001, 6, "1.000001"},
		// A fraction whose digits are shorter than `decimals` must be
		// left-zero-padded, not left-shifted (0.000005, never 0.5).
		{"usdc leading-zero fraction", 1_000_005, 6, "1.000005"},

		// 8 decimals — the wBTC-class scale.
		{"8dp one", 100_000_000, 8, "1.00000000"},
		{"8dp one base unit", 1, 8, "0.00000001"},
		{"8dp mixed", 123_456_789, 8, "1.23456789"},

		// 9 decimals — SOL and the #817 1000x-vs-6dp mint class.
		{"9dp ten", 10_000_000_000, 9, "10.000000000"},
		{"9dp one base unit", 1, 9, "0.000000001"},
		{"9dp nineteen ninety nine", 19_990_000_000, 9, "19.990000000"},

		// Extremes: the registry's declared bounds and uint64 saturation.
		{"min decimals", 15, config.MinTokenDecimals, "1.5"},
		{"max decimals", 1, config.MaxTokenDecimals, "0.000000000000000001"},
		{"max uint64 at 9dp", 18_446_744_073_709_551_615, 9, "18446744073.709551615"},

		// Defensive: ValidateTokenDecimals rejects <1 upstream, but the
		// formatter must never emit a bare "." if it ever sees one.
		{"zero decimals", 1_000_000, 0, "1000000"},
	}

	for _, c := range cases {
		got := FormatBaseUnits(c.units, c.decimals)
		if got != c.want {
			t.Fatalf("%s: FormatBaseUnits(%d, %d) = %q, want %q", c.name, c.units, c.decimals, got, c.want)
		}
		if c.decimals > 0 {
			if _, frac, ok := strings.Cut(got, "."); !ok || len(frac) != c.decimals {
				t.Fatalf("%s: %q must carry exactly %d fractional digits", c.name, got, c.decimals)
			}
		}
	}
}

// TestSolanaPayAmountWirePin walks the whole live chain — fiat micros in, the
// `amount=` query param out — for the decimals a merchant actually runs. The
// point of the chain (rather than the formatter alone) is that a 10^n slip
// anywhere between micros and the wallet shows up here as a wrong dollar value.
func TestSolanaPayAmountWirePin(t *testing.T) {
	t.Parallel()

	const (
		recipient = "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"
		mint      = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
		reference = "11111111111111111111111111111112"
	)
	s := &SolanaPayService{}

	cases := []struct {
		name     string
		micros   moneyutil.Micros
		decimals int
		want     string
	}{
		{"$1 on a 6-decimal mint", 1_000_000, 6, "1.000000"},
		{"$19.99 on a 6-decimal mint", 19_990_000, 6, "19.990000"},
		{"$0.01 on a 6-decimal mint", 10_000, 6, "0.010000"},
		// Same dollars, 9-decimal mint => 1000x the base units and three
		// more digits, but the SAME dollar amount on the wire (#817).
		{"$1 on a 9-decimal mint", 1_000_000, 9, "1.000000000"},
		{"$19.99 on a 9-decimal mint", 19_990_000, 9, "19.990000000"},
		{"$19.99 on an 8-decimal mint", 19_990_000, 8, "19.99000000"},
	}

	for _, c := range cases {
		units, err := FiatMicrosToBaseUnitsAtPeg(c.micros, "USDC", c.decimals)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		url := s.buildTransferRequestURL(context.Background(), recipient, units, c.decimals, mint, "USDC", reference, "")
		wantURL := "solana:" + recipient +
			"?amount=" + c.want +
			"&spl-token=" + mint +
			"&reference=" + reference +
			"&label=Purchase"
		if url != wantURL {
			t.Fatalf("%s: got %q, want %q", c.name, url, wantURL)
		}
	}
}
