package checkout

import (
	"testing"
	"time"
)

func intPtr(v int) *int { return &v }

// TestCalculateModelBUpgradeCharge pins the live card-billing math for #268.
// Model B: first_charge = new_full - old_unused, where
// old_unused = old_full * (hoursRemaining / cycleHours) with integer math,
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
			// hoursRemaining = 672, old_unused = 2000*672/720 = 1866,
			// first_charge = 5000 - 1866 = 3134.
			name:        "issue example $20->$50 28 days remaining",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 3134,
			expectCycle: 30 * 24,
		},
		{
			// Boundary: 0 hours remaining => no credit => first_charge = new_full.
			name:        "zero hours remaining charges full new price",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now), // not After(now) => 0 hours
			cycleHours:  intPtr(30 * 24),
			expectFirst: 5000,
			expectCycle: 30 * 24,
		},
		{
			// Boundary: full period remaining => first_charge = new_full - old_full.
			name:        "full period remaining charges difference",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now.Add(30 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 3000, // 5000 - (2000*30/30=2000)
			expectCycle: 30 * 24,
		},
		{
			// nil periodEnd => 0 hours remaining => full new price.
			name:        "nil period end charges full new price",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   nil,
			cycleHours:  intPtr(30 * 24),
			expectFirst: 5000,
			expectCycle: 30 * 24,
		},
		{
			// periodEnd in the past => 0 hours remaining => full new price.
			name:        "past period end charges full new price",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now.Add(-5 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 5000,
			expectCycle: 30 * 24,
		},
		{
			// Default 30-day (720h) cycle when billingCycleHours is nil.
			name:        "nil cycle hours defaults to 720",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  nil,
			expectFirst: 3134,
			expectCycle: 30 * 24,
		},
		{
			// Non-positive cycle hours falls back to default 720.
			name:        "zero cycle hours defaults to 720",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  intPtr(0),
			expectFirst: 3134,
			expectCycle: 30 * 24,
		},
		{
			// Annual cycle: 365 days, half remaining (182 whole days).
			// old_unused = 10000 * 182 / 365 = 4986 (integer).
			// first_charge = 30000 - 4986 = 25014.
			name:        "annual cycle half remaining",
			oldFull:     10000,
			newFull:     30000,
			periodEnd:   timePtr(now.Add(182 * 24 * time.Hour)),
			cycleHours:  intPtr(365 * 24),
			expectFirst: 25014,
			expectCycle: 365 * 24,
		},
		{
			// Defensive clamp: if new_full < old_full (not a real upgrade),
			// first_charge clamps to >= 0 rather than going negative.
			name:        "downgrade-like inputs clamp to zero",
			oldFull:     5000,
			newFull:     2000,
			periodEnd:   timePtr(now.Add(28 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 0, // 2000 - (5000*28/30=4666) = -2666 -> clamp 0
			expectCycle: 30 * 24,
		},
		{
			// hoursRemaining capped at cycleHours: a period end far beyond one
			// cycle never credits more than a full cycle of old plan.
			name:        "hours remaining exceeding cycle is capped",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now.Add(100 * 24 * time.Hour)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 3000, // capped at 30 days => old_unused=2000 => 3000
			expectCycle: 30 * 24,
		},
		{
			// Partial hours round down. 28 days + 23h59m => 695 whole hours.
			name:        "partial hour rounds down",
			oldFull:     2000,
			newFull:     5000,
			periodEnd:   timePtr(now.Add(28*24*time.Hour + 23*time.Hour + 59*time.Minute)),
			cycleHours:  intPtr(30 * 24),
			expectFirst: 3070,
			expectCycle: 30 * 24,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first, cycle := CalculateModelBUpgradeCharge(tt.oldFull, tt.newFull, tt.periodEnd, tt.cycleHours, now)
			if first != tt.expectFirst {
				t.Fatalf("first charge: expected %d, got %d", tt.expectFirst, first)
			}
			if cycle != tt.expectCycle {
				t.Fatalf("cycle hours: expected %d, got %d", tt.expectCycle, cycle)
			}
			if first < 0 {
				t.Fatalf("first charge must never be negative, got %d", first)
			}
		})
	}
}

// TestModelBNewPeriodEnd documents the period-reset contract used by the
// NMI/Stripe upgrade paths: the new period is [now, now+cycleHours].
func TestModelBNewPeriodEnd(t *testing.T) {
	now := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	_, cycle := CalculateModelBUpgradeCharge(
		2000, 5000,
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
