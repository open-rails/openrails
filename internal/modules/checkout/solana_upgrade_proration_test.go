package checkout

import (
	"context"
	"testing"
	"time"

	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
)

// TestSolanaUpgradeReducedFirstCharge composes the two pure functions the Solana
// Model-B upgrade orchestration chains (#267): CalculateModelBUpgradeCharge to get
// the reduced first charge in CENTS, then FiatCentsToStablecoinBaseUnits to get the
// on-chain first pull in USDC base units. It pins that an upgrade computes the
// RIGHT reduced amount (not the full new price) and that a non-upgrade / clamped
// case behaves correctly.
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
