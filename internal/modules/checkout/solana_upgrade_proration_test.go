package checkout

import (
	"context"
	"testing"
	"time"

	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
)

// TestSolanaUpgradeReducedFirstCharge composes the two pure functions the Solana
// tier-change PREPARE endpoint chains for an UPGRADE (#272, math shared with the
// #267/#268 Model-B policy): CalculateModelBUpgradeCharge to get the reduced first
// charge in CENTS, then FiatCentsToStablecoinBaseUnits to get the on-chain
// FirstChargeBaseUnits in USDC base units. It pins that an upgrade composes the
// RIGHT reduced amount (not the full new price) into PrepareTierChangeInput, and
// that the clamped/no-credit cases behave correctly.
func TestSolanaUpgradeReducedFirstCharge(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cycle := 30

	tests := []struct {
		name          string
		oldFull       int64 // cents
		newFull       int64 // cents
		periodEnd     *time.Time
		wantFirstCent int64
		wantBaseUnits uint64
	}{
		{
			// Issue example: $20 -> $50, 28 days remaining of a 30-day cycle.
			// first = 5000 - (2000*28/30=1866) = 3134c => 31_340_000 base units.
			name:          "upgrade 20 to 50 with 28 days left",
			oldFull:       2000,
			newFull:       5000,
			periodEnd:     timePtr(now.Add(28 * 24 * time.Hour)),
			wantFirstCent: 3134,
			wantBaseUnits: 31_340_000,
		},
		{
			// Zero days remaining => charge full new price.
			name:          "no days remaining charges full new price",
			oldFull:       2000,
			newFull:       5000,
			periodEnd:     timePtr(now),
			wantFirstCent: 5000,
			wantBaseUnits: 50_000_000,
		},
		{
			// Full period remaining => first = newFull - oldFull = 3000c.
			name:          "full period remaining charges difference",
			oldFull:       2000,
			newFull:       5000,
			periodEnd:     timePtr(now.Add(30 * 24 * time.Hour)),
			wantFirstCent: 3000,
			wantBaseUnits: 30_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstCents, gotCycle := CalculateModelBUpgradeCharge(tt.oldFull, tt.newFull, tt.periodEnd, &cycle, now)
			if firstCents != tt.wantFirstCent {
				t.Fatalf("first charge cents: expected %d, got %d", tt.wantFirstCent, firstCents)
			}
			if gotCycle != cycle {
				t.Fatalf("cycle: expected %d, got %d", cycle, gotCycle)
			}
			// The reduced first charge is never the full new price unless no credit applies.
			if tt.wantFirstCent < tt.newFull && firstCents >= tt.newFull {
				t.Fatalf("expected a REDUCED first charge below the full new price")
			}
			gotUnits := solanamodule.FiatCentsToStablecoinBaseUnits(ctx, firstCents, "USDC", nil)
			if gotUnits != tt.wantBaseUnits {
				t.Fatalf("first pull base units: expected %d, got %d", tt.wantBaseUnits, gotUnits)
			}
		})
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
	cycle := 30

	// A "downgrade" (newFull < oldFull) routed through the Model-B charge clamps to
	// 0 cents, and the prepare endpoint additionally SKIPS this computation for a
	// downgrade entirely — either way the composed first charge is zero.
	firstCents, _ := CalculateModelBUpgradeCharge(5000, 2000, timePtr(now.Add(15*24*time.Hour)), &cycle, now)
	if firstCents != 0 {
		t.Fatalf("a downgrade must not produce a positive Model-B first charge, got %d cents", firstCents)
	}
	gotUnits := solanamodule.FiatCentsToStablecoinBaseUnits(context.Background(), firstCents, "USDC", nil)
	if gotUnits != 0 {
		t.Fatalf("downgrade FirstChargeBaseUnits must be 0, got %d", gotUnits)
	}
}
