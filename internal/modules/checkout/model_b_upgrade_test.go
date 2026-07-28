package checkout

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func intPtr(v int) *int { return &v }

// usd tags a micro amount with its currency for the Model-B helper (#820).
func usd(micros int64) PriceAmount { return PriceAmount{Micros: micros, Currency: "usd"} }

// TestCalculateModelBUpgradeCharge pins the live card-billing math for #268.
// Model B: first_charge = new_full - old_unused, where
// old_unused = ceilToCent(old_full * hoursRemaining / cycleHours) with integer
// math (#671: inputs/outputs are MICROS; the credit rounds UP to a whole cent,
// customer-favored, so whole-cent prices yield a whole-cent charge),
// hoursRemaining = whole hours left in the current paid period, clamped >= 0.
func TestCalculateModelBUpgradeCharge(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		oldFull     int64
		newFull     int64
		periodEnd   *time.Time
		cycleHours  *int
		expectFirst int64
		expectCycle int
	}{
		{
			// Issue example: $20 -> $50, 2 days into a 30-day cycle.
			// hoursRemaining = 672, old_unused = 20_000_000*672/720 = 18_666_666
			// -> ceil to cent 18_670_000, first = 50_000_000-18_670_000.
			name:        "issue example $20->$50 28 days remaining",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 31_330_000,
			expectCycle: 30 * 24,
		},
		{
			// Boundary: 0 hours remaining => no credit => first_charge = new_full.
			name:        "zero hours remaining charges full new price",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now), // not After(now) => 0 hours
			cycleHours:  intPtr(30 * 24),
			expectFirst: 50_000_000,
			expectCycle: 30 * 24,
		},
		{
			// Boundary: full period remaining => first_charge = new_full - old_full.
			name:        "full period remaining charges difference",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now.Add(30 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 30_000_000, // credit is exactly $20 (whole cents already)
			expectCycle: 30 * 24,
		},
		{
			// nil periodEnd => 0 hours remaining => full new price.
			name:        "nil period end charges full new price",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   nil,
			cycleHours:  intPtr(30 * 24),
			expectFirst: 50_000_000,
			expectCycle: 30 * 24,
		},
		{
			// periodEnd in the past => 0 hours remaining => full new price.
			name:        "past period end charges full new price",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now.Add(-5 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 50_000_000,
			expectCycle: 30 * 24,
		},
		{
			// Default 30-day (720h) cycle when billingCycleHours is nil.
			name:        "nil cycle hours defaults to 720",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  nil,
			expectFirst: 31_330_000,
			expectCycle: 30 * 24,
		},
		{
			// Non-positive cycle hours falls back to default 720.
			name:        "zero cycle hours defaults to 720",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  intPtr(0),
			expectFirst: 31_330_000,
			expectCycle: 30 * 24,
		},
		{
			// Annual cycle: 365 days, half remaining (182 whole days).
			// old_unused = 100_000_000*4368/8760 = 49_863_013 -> ceil 49_870_000.
			name:        "annual cycle half remaining",
			oldFull:     100_000_000,
			newFull:     300_000_000,
			periodEnd:   timePtr(now.Add(182 * 24 * time.Hour)),
			cycleHours:  intPtr(365 * 24),
			expectFirst: 250_130_000,
			expectCycle: 365 * 24,
		},
		{
			// Defensive clamp: if new_full < old_full (not a real upgrade),
			// first_charge clamps to >= 0 rather than going negative.
			name:        "downgrade-like inputs clamp to zero",
			oldFull:     50_000_000,
			newFull:     20_000_000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 0,
			expectCycle: 30 * 24,
		},
		{
			// hoursRemaining capped at cycleHours: a period end far beyond one
			// cycle never credits more than a full cycle of old plan.
			name:        "hours remaining exceeding cycle is capped",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now.Add(100 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 30_000_000, // capped at 30 days => old_unused=20_000_000
			expectCycle: 30 * 24,
		},
		{
			// Partial hours round down. 28 days + 23h59m => 695 whole hours.
			// old_unused = 20_000_000*695/720 = 19_305_555 -> ceil 19_310_000.
			name:        "partial hour rounds down",
			oldFull:     20_000_000,
			newFull:     50_000_000,
			periodEnd:   timePtr(now.Add(28*24*time.Hour + 23*time.Hour + 59*time.Minute)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 30_690_000,
			expectCycle: 30 * 24,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, cycle, err := CalculateModelBUpgradeCharge(usd(tt.oldFull), usd(tt.newFull), tt.periodEnd, tt.cycleHours, now)
			if err != nil {
				t.Fatalf("same-currency proration must not error: %v", err)
			}
			if first != tt.expectFirst {
				t.Fatalf("first charge: expected %d, got %d", tt.expectFirst, first)
			}
			if cycle != tt.expectCycle {
				t.Fatalf("cycle hours: expected %d, got %d", tt.expectCycle, cycle)
			}
			if first < 0 {
				t.Fatalf("first charge must never be negative, got %d", first)
			}
			// #671 invariant: whole-cent inputs => whole-cent first charge,
			// exactly chargeable on cent-based rails.
			if _, err := moneyutil.MicrosToCentsExact(moneyutil.Micros(first)); err != nil {
				t.Fatalf("first charge %d micros is not whole cents: %v", first, err)
			}
		})
	}
}

// TestUpgradeWirePinning pins the LITERAL provider amounts of an NMI Model-B
// upgrade for a known price (#671): the preview micros, the RunSale proration
// (cents -> "31.33" on the wire) and the recurring subscription amount
// (dollars) must all describe the same money. A micros/cents mixup here fails
// this test, not production.
func TestUpgradeWirePinning(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	cycle := 30 * 24

	// $20 -> $50 with 28 of 30 days left.
	firstMicros, _, _ := CalculateModelBUpgradeCharge(usd(20_000_000), usd(50_000_000), timePtr(now.Add(28*24*time.Hour)), &cycle, now)
	if firstMicros != 31_330_000 {
		t.Fatalf("preview micros: expected 31_330_000, got %d", firstMicros)
	}

	// The RunSale seam converts micros -> CENTS exactly (never raw micros).
	cents, err := moneyutil.MicrosToCentsExact(moneyutil.Micros(firstMicros))
	if err != nil {
		t.Fatalf("proration must be whole cents: %v", err)
	}
	if cents != 3133 {
		t.Fatalf("proration cents: expected 3133, got %d", cents)
	}
	// nmi.SaleParams.Amount is cents; the client posts FormatCentsDecimal(cents).
	if wire := moneyutil.FormatCentsDecimal(cents); wire != "31.33" {
		t.Fatalf("NMI sale wire amount: expected %q, got %q", "31.33", wire)
	}

	// The recurring enrollment amount is CENTS (#818): create and upgrade paths
	// share MicrosToCentsExact, so the same price yields the same wire value.
	for micros, want := range map[int64]string{19_990_000: "19.99", 50_000_000: "50.00"} {
		c, err := moneyutil.MicrosToCentsExact(moneyutil.Micros(micros))
		if err != nil {
			t.Fatalf("recurring cents for %d micros: %v", micros, err)
		}
		if wire := moneyutil.FormatCentsDecimal(c); wire != want {
			t.Fatalf("recurring wire amount: expected %q, got %q", want, wire)
		}
	}

	// A price not representable in whole cents must ERROR at the sale seam,
	// never round: 0 hours remaining => first charge = newFull = sub-cent.
	subCent, _, _ := CalculateModelBUpgradeCharge(usd(20_000_000), usd(50_000_001), timePtr(now), &cycle, now)
	if _, err := moneyutil.MicrosToCentsExact(moneyutil.Micros(subCent)); err == nil {
		t.Fatalf("sub-cent proration must error, got cents for %d micros", subCent)
	}
}

// TestModelBNewPeriodEnd documents the period-reset contract used by the
// NMI/Stripe upgrade paths: the new period is [now, now+cycleHours].
func TestModelBNewPeriodEnd(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	_, cycle, _ := CalculateModelBUpgradeCharge(
		usd(20_000_000), usd(50_000_000),
		timePtr(now.Add(28*24*time.Hour)),
		intPtr(30*24),
		now,
	)
	gotEnd := now.Add(time.Duration(cycle) * time.Hour)
	wantEnd := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	if !gotEnd.Equal(wantEnd) {
		t.Fatalf("new period end: expected %v, got %v", wantEnd, gotEnd)
	}
}
