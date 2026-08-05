package models

import "testing"

// or#893: this is the Go mirror of the DB CHECKs payments_psp_required_on_rail
// and invoice_payments_psp_required_on_rail. If the two ever disagree, a write
// the repo waves through fails at the constraint instead — so the vocabulary is
// pinned here rather than left to a string literal at each call site.
func TestIsOffRailChannel(t *testing.T) {
	for _, rail := range []string{"manual", "admin", "MANUAL", " Admin "} {
		if !IsOffRailChannel(rail) {
			t.Errorf("IsOffRailChannel(%q) = false, want true", rail)
		}
	}
	for _, rail := range []string{"nmi", "stripe", "ccbill", "solana", "mobius", ""} {
		if IsOffRailChannel(rail) {
			t.Errorf("IsOffRailChannel(%q) = true, want false — a real rail must carry a PSP", rail)
		}
	}
}
