package service

import (
	"testing"

	"github.com/open-rails/openrails/internal/modules/credits"
)

// TestRemainingTodayCents picks the daily-cap headroom (org or per-invoker) from
// the evaluated caps for the authorize response, and clamps negatives to zero.
func TestRemainingTodayCents(t *testing.T) {
	if r := remainingTodayCents(nil); r != nil {
		t.Fatalf("no caps => nil, got %v", *r)
	}

	// Only a monthly cap present: no daily headroom reported.
	monthly := []credits.CapResult{{Code: credits.DenyMonthlyCap, Remaining: 500}}
	if r := remainingTodayCents(monthly); r != nil {
		t.Fatalf("monthly-only => nil, got %v", *r)
	}

	// Org daily cap: report its remaining.
	org := []credits.CapResult{
		{Code: credits.DenyMonthlyCap, Remaining: 9000},
		{Code: credits.DenyDailyCap, Remaining: 300},
	}
	r := remainingTodayCents(org)
	if r == nil || *r != 300 {
		t.Fatalf("org daily remaining = %v, want 300", r)
	}

	// Per-invoker daily cap also counts as "today".
	inv := []credits.CapResult{{Code: credits.DenyInvokerDailyCap, Remaining: 42}}
	r = remainingTodayCents(inv)
	if r == nil || *r != 42 {
		t.Fatalf("invoker daily remaining = %v, want 42", r)
	}

	// Negative remaining (over cap) clamps to zero.
	over := []credits.CapResult{{Code: credits.DenyDailyCap, Remaining: -50}}
	r = remainingTodayCents(over)
	if r == nil || *r != 0 {
		t.Fatalf("over-cap remaining = %v, want 0", r)
	}
}
