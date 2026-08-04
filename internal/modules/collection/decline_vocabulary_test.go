package collection

import (
	"testing"
	"time"
)

// or#870: WIRE-PIN the rail decline vocabularies.
//
// These tables are policy, not data: every row decides whether a real customer
// keeps charging, keeps access, or gets cancelled. A silent edit to one of them
// changes production behaviour with no other symptom — the money still moves,
// the logs still look ordinary. So the tables are pinned exactly: the full
// expected mapping is written out here, and any addition, removal or bucket
// change must be made in TWO places by someone who meant it.

// TestStripeVocabularyIsPinned pins Stripe's decline_code mapping in full.
func TestStripeVocabularyIsPinned(t *testing.T) {
	want := map[string]DeclineOutcome{
		// Bucket 2 — their card, fixable. Stop charging, keep access.
		"expired_card":            DeclineFixPaymentMethod,
		"invalid_expiry_month":    DeclineFixPaymentMethod,
		"invalid_expiry_year":     DeclineFixPaymentMethod,
		"incorrect_cvc":           DeclineFixPaymentMethod,
		"invalid_cvc":             DeclineFixPaymentMethod,
		"incorrect_zip":           DeclineFixPaymentMethod,
		"incorrect_number":        DeclineFixPaymentMethod,
		"invalid_number":          DeclineFixPaymentMethod,
		"incorrect_pin":           DeclineFixPaymentMethod,
		"invalid_pin":             DeclineFixPaymentMethod,
		"pin_try_exceeded":        DeclineFixPaymentMethod,
		"card_not_supported":      DeclineFixPaymentMethod,
		"currency_not_supported":  DeclineFixPaymentMethod,
		"do_not_honor":            DeclineFixPaymentMethod,
		"transaction_not_allowed": DeclineFixPaymentMethod,
		"call_issuer":             DeclineFixPaymentMethod,
		"authentication_required": DeclineFixPaymentMethod,

		// Bucket 3 — non-recoverable. Every row here is a cancellation.
		"revocation_of_authorization":      DeclineNonRecoverable,
		"revocation_of_all_authorizations": DeclineNonRecoverable,
		"stop_payment_order":               DeclineNonRecoverable,
		"lost_card":                        DeclineNonRecoverable,
		"stolen_card":                      DeclineNonRecoverable,
		"pickup_card":                      DeclineNonRecoverable,
		"fraudulent":                       DeclineNonRecoverable,
		"invalid_account":                  DeclineNonRecoverable,
		"no_account":                       DeclineNonRecoverable,
	}
	assertVocabularyPinned(t, "stripe", want, stripeDeclineOutcomes)

	// The money-shaped Stripe declines that MUST keep dunning. Naming them
	// explicitly is the half a table of non-retry rows cannot express: an
	// accidental bucket-2 row for insufficient_funds would stop charging every
	// customer who was merely short this month.
	for _, code := range []string{
		"insufficient_funds", "generic_decline", "card_declined", "processing_error",
		"issuer_not_available", "try_again_later", "duplicate_transaction",
		"card_velocity_exceeded", "withdrawal_count_limit_exceeded",
	} {
		if got := ClassifyDecline("stripe", code); got != DeclineRetry {
			t.Errorf("stripe %q = %q, want retry — this code must never stop charging", code, got)
		}
		if got := ClassifyDeclineDetail("stripe", code).Coverage; got != CoverageKnownRetry {
			t.Errorf("stripe %q coverage = %q, want known_retry (it is a DECISION, not a gap)", code, got)
		}
	}
}

// TestCCBillVocabularyIsPinned pins CCBill's BE-nnn mapping in full.
func TestCCBillVocabularyIsPinned(t *testing.T) {
	want := map[string]DeclineOutcome{
		"be102": DeclineFixPaymentMethod, // pickup card
		"be103": DeclineFixPaymentMethod, // do not honor
		"be107": DeclineFixPaymentMethod, // invalid credit card
		"be114": DeclineFixPaymentMethod, // expired card
		"be116": DeclineFixPaymentMethod, // service not allowed
		"be132": DeclineFixPaymentMethod, // card blocked (CCBill)
		"be146": DeclineFixPaymentMethod, // blocked country (CCBill)
	}
	assertVocabularyPinned(t, "ccbill", want, ccbillDeclineOutcomes)
}

// TestCCBillHasNoNonRecoverableBucket is the deliberate restraint, asserted.
//
// A bucket-3 row terminally cancels a paying customer. No CCBill code has been
// observed on live traffic in this codebase, so none has earned that. BE-112
// (No Account) is the plausible candidate — Stripe's `no_account` IS bucket 3 —
// and it stays in bucket 1 until a live webhook proves the meaning. If someone
// adds a CCBill cancellation row, they have to delete this test to do it.
func TestCCBillHasNoNonRecoverableBucket(t *testing.T) {
	for code, outcome := range ccbillDeclineOutcomes {
		if outcome == DeclineNonRecoverable {
			t.Errorf("ccbill %q is bucket 3: no CCBill code is live-verified enough to justify a terminal cancellation", code)
		}
	}
	if got := ClassifyDecline("ccbill", "BE-112"); got != DeclineRetry {
		t.Errorf("ccbill BE-112 (no account) = %q, want retry until a live webhook proves the meaning", got)
	}
}

// TestCCBillCodeShapesAreOneRow — CCBill's code arrives verbatim off the wire
// and its punctuation is not load-bearing. Three spellings of the same decline
// must not be three different decisions.
func TestCCBillCodeShapesAreOneRow(t *testing.T) {
	for _, shape := range []string{"BE-114", "be-114", "BE114", "be114", " BE-114 "} {
		if got := ClassifyDecline("ccbill", shape); got != DeclineFixPaymentMethod {
			t.Errorf("ccbill %q = %q, want fix_payment_method — the same code in another shape", shape, got)
		}
	}
}

// TestUnmappedCodesAreDistinguishableFromDecidedRetries is the whole point of
// DeclineCoverage. Both answers are bucket 1, and the outcome alone cannot tell
// them apart — which is exactly why a rail could add a "stop all recurring"
// code and we would retry it forever with no symptom.
func TestUnmappedCodesAreDistinguishableFromDecidedRetries(t *testing.T) {
	cases := []struct {
		rail, code string
		want       DeclineCoverage
	}{
		// Bucketed: an explicit row.
		{"nmi", "223", CoverageBucketed},
		{"nmi", "nmi_expired_card", CoverageBucketed},
		{"nmi", "nmi_response_262", CoverageBucketed},
		{"stripe", "stolen_card", CoverageBucketed},
		{"ccbill", "BE-114", CoverageBucketed},
		// Decided retries: recognized, deliberately bucket 1.
		{"nmi", "202", CoverageKnownRetry},
		{"nmi", "410", CoverageKnownRetry},
		{"stripe", "insufficient_funds", CoverageKnownRetry},
		{"ccbill", "BE-113", CoverageKnownRetry},
		{"ccbill", "BE-950", CoverageKnownRetry}, // the documented 900..999 system-error range
		// Gaps: a rail we DO read, returning a code nobody mapped.
		{"nmi", "999", CoverageUnrecognized},
		{"nmi", "brand_new_localization_id", CoverageUnrecognized},
		{"stripe", "some_code_stripe_added_last_tuesday", CoverageUnrecognized},
		{"ccbill", "BE-777", CoverageUnrecognized},
		// Nothing to map: no code, or a rail with no vocabulary at all.
		{"nmi", "", CoverageNoVocabulary},
		{"solana", "anything", CoverageNoVocabulary},
		{"vaulted_card", "223", CoverageNoVocabulary}, // retired value (or#879)
		{"a_rail_that_does_not_exist", "252", CoverageNoVocabulary},
	}
	for _, c := range cases {
		got := ClassifyDeclineDetail(c.rail, c.code)
		if got.Coverage != c.want {
			t.Errorf("ClassifyDeclineDetail(%q, %q).Coverage = %q, want %q", c.rail, c.code, got.Coverage, c.want)
		}
		// Whatever the coverage, an unbucketed code is bucket 1. Never 2 or 3.
		if c.want != CoverageBucketed && got.Outcome != DeclineRetry {
			t.Errorf("ClassifyDeclineDetail(%q, %q) = %q; an unbucketed code must be retry", c.rail, c.code, got.Outcome)
		}
		if wantAlert := c.want == CoverageUnrecognized; got.NeedsMapping() != wantAlert {
			t.Errorf("ClassifyDeclineDetail(%q, %q).NeedsMapping() = %v, want %v", c.rail, c.code, got.NeedsMapping(), wantAlert)
		}
	}
}

// TestFailureActionCarriesCoverage — the invoice consumer only has the Action,
// so the gap has to survive the trip or it cannot be reported.
func TestFailureActionCarriesCoverage(t *testing.T) {
	now := time.Now()
	code := "a_code_no_rail_ever_published"
	a := FailureAction(MonthlyCycleHours, "stripe", &code, 0, nil, now)
	if !a.Decline.NeedsMapping() {
		t.Fatalf("FailureAction dropped the coverage answer: %+v", a.Decline)
	}
	if a.Outcome != DeclineRetry || a.NextAttemptAt == nil {
		t.Errorf("an unmapped code must keep the schedule, got %+v", a)
	}

	known := "insufficient_funds"
	if b := FailureAction(MonthlyCycleHours, "stripe", &known, 0, nil, now); b.Decline.NeedsMapping() {
		t.Error("insufficient_funds is a decided retry, not an unmapped code")
	}
}

// TestPaymentMethodNoticeLadder pins the rung schedule and the two properties
// that make it safe: it always terminates, and it terminates by running out —
// never by deciding anything about the subscription.
func TestPaymentMethodNoticeLadder(t *testing.T) {
	parked := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

	if got := PaymentMethodNoticeRungs(); got != 3 {
		t.Fatalf("ladder has %d rungs, want 3 (immediate, +3d, +10d)", got)
	}

	// Rung 1 is sent inline by the failure flow, so the ladder asks for rung 2
	// at +3d and rung 3 at +10d — both anchored to the PARK, not to each other.
	for _, c := range []struct {
		rungsSent int
		want      time.Time
		ok        bool
	}{
		{1, parked.Add(3 * 24 * time.Hour), true},
		{2, parked.Add(10 * 24 * time.Hour), true},
		{3, time.Time{}, false}, // spent
		{0, time.Time{}, false}, // a ladder is opened WITH its first rung sent
	} {
		got, ok := NextPaymentMethodNoticeAt(c.rungsSent, parked)
		if ok != c.ok || (ok && !got.Equal(c.want)) {
			t.Errorf("NextPaymentMethodNoticeAt(%d) = (%v, %v), want (%v, %v)", c.rungsSent, got, ok, c.want, c.ok)
		}
	}

	// Only the last rung says it is the last one.
	if IsFinalPaymentMethodNotice(1) {
		t.Error("rung 2 of 3 must not be announced as final")
	}
	if !IsFinalPaymentMethodNotice(2) {
		t.Error("rung 3 of 3 is the final notice")
	}
}

// assertVocabularyPinned compares a table to its pin in both directions, so a
// removed row fails as loudly as an added one.
func assertVocabularyPinned(t *testing.T, rail string, want map[string]DeclineOutcome, got map[string]DeclineOutcome) {
	t.Helper()
	for code, wantOutcome := range want {
		gotOutcome, ok := got[code]
		if !ok {
			t.Errorf("%s: %q was REMOVED from the table; if that is intended, change the pin", rail, code)
			continue
		}
		if gotOutcome != wantOutcome {
			t.Errorf("%s: %q moved to bucket %q, pinned as %q", rail, code, gotOutcome, wantOutcome)
		}
		// And through the public entry point, on the real code shape.
		if via := ClassifyDecline(rail, code); via != wantOutcome {
			t.Errorf("%s: ClassifyDecline(%q) = %q, table says %q", rail, code, via, wantOutcome)
		}
	}
	for code := range got {
		if _, ok := want[code]; !ok {
			t.Errorf("%s: %q was ADDED to the table without updating the pin — a new bucket row is a policy change", rail, code)
		}
	}
}
