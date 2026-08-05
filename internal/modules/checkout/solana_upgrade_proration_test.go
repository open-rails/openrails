package checkout

import (
	"context"
	"testing"
	"time"

	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// TestSolanaUpgradeReducedFirstCharge composes the two pure functions the Solana
// tier-change PREPARE endpoint chains for an UPGRADE (#272, math shared with the
// #267/#268 Model-B policy): CalculateModelBUpgradeCharge to get the reduced first
// charge in MICROS, then FiatMicrosToStablecoinBaseUnits to get the on-chain
// FirstChargeBaseUnits in USDC base units (#671). It pins that an upgrade composes
// the RIGHT reduced amount (not the full new price, never a 10,000x cents-misread)
// into PrepareTierChangeInput, and that the clamped/no-credit cases behave correctly.
func TestSolanaUpgradeReducedFirstCharge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cycle := 30 * 24

	tests := []struct {
		name            string
		oldFull         int64 // micros
		newFull         int64 // micros
		periodEnd       *time.Time
		wantFirstMicros int64
		wantBaseUnits   uint64
	}{
		{
			// Issue example: $20 -> $50, 28 days remaining of a 30-day cycle.
			// old_unused = ceilToCent(20_000_000*672/720) = 18_670_000 micros,
			// first = 31_330_000 micros => 31_330_000 USDC base units at the peg.
			name:            "upgrade 20 to 50 with 28 days left",
			oldFull:         20_000_000,
			newFull:         50_000_000,
			periodEnd:       timePtr(now.Add(28 * 24 * time.Hour)),
			wantFirstMicros: 31_330_000,
			wantBaseUnits:   31_330_000,
		},
		{
			// Zero days remaining => charge full new price.
			name:            "no days remaining charges full new price",
			oldFull:         20_000_000,
			newFull:         50_000_000,
			periodEnd:       timePtr(now),
			wantFirstMicros: 50_000_000,
			wantBaseUnits:   50_000_000,
		},
		{
			// Full period remaining => first = newFull - oldFull = $30.
			name:            "full period remaining charges difference",
			oldFull:         20_000_000,
			newFull:         50_000_000,
			periodEnd:       timePtr(now.Add(30 * 24 * time.Hour)),
			wantFirstMicros: 30_000_000,
			wantBaseUnits:   30_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstMicros, gotCycle, err := CalculateModelBUpgradeCharge(usd(tt.oldFull), usd(tt.newFull), tt.periodEnd, &cycle, now)
			if err != nil {
				t.Fatalf("same-currency proration must not error: %v", err)
			}
			if firstMicros != tt.wantFirstMicros {
				t.Fatalf("first charge micros: expected %d, got %d", tt.wantFirstMicros, firstMicros)
			}
			if gotCycle != cycle {
				t.Fatalf("cycle: expected %d, got %d", cycle, gotCycle)
			}
			// The reduced first charge is never the full new price unless no credit applies.
			if tt.wantFirstMicros < tt.newFull && firstMicros >= tt.newFull {
				t.Fatalf("expected a REDUCED first charge below the full new price")
			}
			gotUnits, err := solanamodule.FiatMicrosToStablecoinBaseUnits(ctx, moneyutil.Micros(firstMicros), "USDC", 6, nil)
			if err != nil {
				t.Fatalf("convert first charge: %v", err)
			}
			if gotUnits != tt.wantBaseUnits {
				t.Fatalf("first pull base units: expected %d, got %d", tt.wantBaseUnits, gotUnits)
			}
		})
	}
}

// TestSolanaUpgradeFirstChargeHonoursTokenDecimals pins #817 on the
// recurring/first-charge path: the SAME prorated micros must become different
// base units per the merchant's configured token decimals. The old code
// hardcoded 6, so a 9-decimal mint pulled 1/1000th of the intended charge.
func TestSolanaUpgradeFirstChargeHonoursTokenDecimals(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cycle := 30 * 24

	// $20 -> $50 with 28 days left => 31_330_000 micros (see above).
	firstMicros, _, _ := CalculateModelBUpgradeCharge(usd(20_000_000), usd(50_000_000), timePtr(now.Add(28*24*time.Hour)), &cycle, now)
	if firstMicros != 31_330_000 {
		t.Fatalf("first charge micros: got %d", firstMicros)
	}

	cases := []struct {
		decimals int
		want     uint64
	}{
		{6, 31_330_000},
		{8, 3_133_000_000},
		{9, 31_330_000_000},
	}
	for _, tc := range cases {
		got, err := solanamodule.FiatMicrosToStablecoinBaseUnits(ctx, moneyutil.Micros(firstMicros), "USDC", tc.decimals, nil)
		if err != nil {
			t.Fatalf("@%d decimals: %v", tc.decimals, err)
		}
		if got != tc.want {
			t.Fatalf("@%d decimals: got %d base units, want %d", tc.decimals, got, tc.want)
		}
	}

	// Missing decimals must fail loudly, never silently price at 10^0.
	if _, err := solanamodule.FiatMicrosToStablecoinBaseUnits(ctx, moneyutil.Micros(firstMicros), "USDC", 0, nil); err == nil {
		t.Fatal("expected an error for a token with no configured decimals")
	}
}

// TestSolanaDowngradeHasZeroFirstCharge pins that a DOWNGRADE composes a ZERO
// FirstChargeBaseUnits into PrepareTierChangeInput (#272): there is no immediate
// charge — the first pull is deferred to the old period end. The tier-change
// prepare endpoint only computes the prorated first charge for an UPGRADE; a
// downgrade leaves FirstChargeBaseUnits at its zero value and the prepare service
// builds the unsigned [cancel + subscribe] bundle with no transfer.
func TestSolanaDowngradeHasZeroFirstCharge(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cycle := 30 * 24

	// A "downgrade" (newFull < oldFull) routed through the Model-B charge clamps to
	// 0 micros, and the prepare endpoint additionally SKIPS this computation for a
	// downgrade entirely — either way the composed first charge is zero.
	firstMicros, _, _ := CalculateModelBUpgradeCharge(usd(50_000_000), usd(20_000_000), timePtr(now.Add(15*24*time.Hour)), &cycle, now)
	if firstMicros != 0 {
		t.Fatalf("a downgrade must not produce a positive Model-B first charge, got %d micros", firstMicros)
	}
	gotUnits, err := solanamodule.FiatMicrosToStablecoinBaseUnits(context.Background(), moneyutil.Micros(firstMicros), "USDC", 6, nil)
	if err != nil {
		t.Fatalf("convert first charge: %v", err)
	}
	if gotUnits != 0 {
		t.Fatalf("downgrade FirstChargeBaseUnits must be 0, got %d", gotUnits)
	}
}
