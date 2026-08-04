package payments

import "testing"

// or#870: CCBill's BE-nnn vocabulary, normalized.
//
// CCBill does not emit ISO decline codes — it emits its own alphanumerical
// codes, published at ccbill.com/kb/list-of-credit-card-declined-codes, and
// that string arrives verbatim in the webhook `failureCode` field. Until now
// the table was empty and every CCBill decline read "unknown", which made the
// whole rail invisible to the failure_reason dimension.
//
// Pinned in full: a normalized reason feeds approval-rate analysis and the
// decline doctrine's coverage answer, so a quiet re-categorization is a quiet
// change to both.
func TestCCBillFailureReasonsArePinned(t *testing.T) {
	want := map[string]string{
		"BE-101": FailureConfigError,       // invalid MID/TID — ours, never the customer's
		"BE-102": FailureFraudSuspected,    // pickup card
		"BE-103": FailureCardDeclined,      // do not honor
		"BE-105": FailureProcessorError,    // invalid transaction
		"BE-107": FailureCardDeclined,      // invalid credit card
		"BE-112": FailureCardDeclined,      // no account
		"BE-113": FailureInsufficientFunds, // insufficient funds
		"BE-114": FailureExpiredCard,       // expired card
		"BE-116": FailureCardDeclined,      // service not allowed
		"BE-119": FailureCardDeclined,      // activity limit exceeded
		"BE-130": FailureProcessorError,    // invalid field provided
		"BE-132": FailureCardDeclined,      // card blocked (CCBill)
		"BE-146": FailureCardDeclined,      // blocked country (CCBill)
	}
	for code, reason := range want {
		if got := NormalizeFailureReason("ccbill", code); got != reason {
			t.Errorf("NormalizeFailureReason(ccbill, %q) = %q, want %q", code, got, reason)
		}
	}
	if len(ccbillFailureReasons) != len(want) {
		t.Errorf("ccbillFailureReasons has %d rows, pinned at %d — a new row is a policy change; update the pin",
			len(ccbillFailureReasons), len(want))
	}
}

// TestCCBillSystemErrorRangeIsProcessorError — CCBill documents BE-900..999 as
// one thing ("system error, authorization not successful"). A hundred table
// rows saying the same sentence would be noise, so the range is folded; the
// fold has to actually cover the range.
func TestCCBillSystemErrorRangeIsProcessorError(t *testing.T) {
	for _, code := range []string{"BE-900", "BE-901", "BE-950", "BE-999"} {
		if got := NormalizeFailureReason("ccbill", code); got != FailureProcessorError {
			t.Errorf("NormalizeFailureReason(ccbill, %q) = %q, want %q", code, got, FailureProcessorError)
		}
	}
	// The boundaries are real boundaries, not a prefix match on "be9".
	for _, code := range []string{"BE-899", "BE-1000", "BE-9"} {
		if got := NormalizeFailureReason("ccbill", code); got != FailureUnknown {
			t.Errorf("NormalizeFailureReason(ccbill, %q) = %q, want unknown — outside the documented range", code, got)
		}
	}
}

// TestCCBillCodeShapeIsNotLoadBearing — the code is recorded verbatim off the
// wire, so its punctuation and case must not change the answer.
func TestCCBillCodeShapeIsNotLoadBearing(t *testing.T) {
	for _, shape := range []string{"BE-114", "be-114", "BE114", "be114", " be-114 "} {
		if got := NormalizeFailureReason("ccbill", shape); got != FailureExpiredCard {
			t.Errorf("NormalizeFailureReason(ccbill, %q) = %q, want %q", shape, got, FailureExpiredCard)
		}
	}
}

// TestUnknownCCBillCodeStaysUnknown — the #733 no-fabrication rule. An
// unrecognized code reads "unknown" with the verbatim code preserved beside it;
// it is never guessed into a category.
func TestUnknownCCBillCodeStaysUnknown(t *testing.T) {
	for _, code := range []string{"BE-777", "declined", "", "202"} {
		if got := NormalizeFailureReason("ccbill", code); got != FailureUnknown {
			t.Errorf("NormalizeFailureReason(ccbill, %q) = %q, want unknown", code, got)
		}
	}
}
