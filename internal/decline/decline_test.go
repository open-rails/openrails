package decline

import (
	"slices"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
)

// Every reason a table produces has its cause and action, and is one the
// billing package lists.
func TestEveryMappedReasonHasAPolicy(t *testing.T) {
	listed := billing.DeclineReasons()
	check := func(source string, reason billing.DeclineReason) {
		t.Helper()
		spec, ok := reasons[reason]
		if !ok || spec.cause == "" {
			t.Errorf("%s maps to %q, which has no cause", source, reason)
		}
		if !slices.Contains(billing.DeclineCauses(), spec.cause) {
			t.Errorf("%q has cause %q, not a billing.DeclineCause", reason, spec.cause)
		}
		if !slices.Contains(listed, reason) {
			t.Errorf("%s maps to %q, which billing.DeclineReasons omits", source, reason)
		}
	}
	for rail, table := range rails {
		for code, reason := range table.codes {
			check(rail+" "+code, reason)
		}
	}
	for code, reason := range issuerCodes {
		check("issuer "+code, reason)
	}
	for reason := range reasons {
		check("reasons", reason)
	}
}

func TestIssuerCodeRefinesOnlyUnspecificReasons(t *testing.T) {
	for _, tc := range []struct {
		code, issuer string
		reason       billing.DeclineReason
		action       Action
	}{
		{"200", "51", billing.DeclineInsufficientFunds, Retry},
		{"201", "59", billing.DeclineSuspectedFraud, Retry},
		// Visa category 1: never charged again, and never terminal from an issuer code.
		{"200", "R1", billing.DeclineStopRecurring, FixPaymentMethod},
		{"200", "43", billing.DeclineStolenCard, FixPaymentMethod},
		{"202", "05", billing.DeclineInsufficientFunds, Retry},
		{"200", "", billing.DeclineGeneric, Retry},
	} {
		r := ClassifyEvidence(Evidence{Rail: "nmi", Code: tc.code, IssuerCode: tc.issuer})
		if r.Reason != tc.reason || r.Action != tc.action {
			t.Errorf("NMI %s with issuer %q: %s/%s, want %s/%s", tc.code, tc.issuer, r.Reason, r.Action, tc.reason, tc.action)
		}
	}
}

func TestCauseDecidesOutcome(t *testing.T) {
	for _, tc := range []struct {
		rail, code string
		outcome    billing.AttemptOutcome
		cause      billing.DeclineCause
	}{
		{"nmi", "202", billing.AttemptDeclined, billing.DeclineCauseCustomer},
		{"nmi", "201", billing.AttemptDeclined, billing.DeclineCauseIssuer},
		{"nmi", "253", billing.AttemptDeclined, billing.DeclineCauseFraud},
		{"nmi", "300", billing.AttemptRefused, billing.DeclineCauseMerchant},
		{"nmi", "461", billing.AttemptRefused, billing.DeclineCauseMerchant},
		{"nmi", "420", billing.AttemptError, billing.DeclineCauseProcessing},
		{"nmi", "299", billing.AttemptDeclined, billing.DeclineCauseUnknown},
		{"stripe", "highest_risk_level", billing.AttemptRefused, billing.DeclineCauseMerchant},
		{"ccbill", "BE-113", billing.AttemptDeclined, billing.DeclineCauseCustomer},
		{"solana", "insufficient_funds", billing.AttemptDeclined, billing.DeclineCauseCustomer},
	} {
		r := Classify(tc.rail, tc.code)
		if r.Outcome() != tc.outcome || r.Cause != tc.cause {
			t.Errorf("%s %s: %s/%s, want %s/%s", tc.rail, tc.code, r.Outcome(), r.Cause, tc.outcome, tc.cause)
		}
	}
}

func TestNetworkAdvice(t *testing.T) {
	for _, tc := range []struct {
		advice, network string
		reason          billing.DeclineReason
		action          Action
		after           time.Duration
	}{
		{"", "01", billing.DeclineUpdateCardholderData, FixPaymentMethod, 0},
		{"confirm_card_data", "", billing.DeclineUpdateCardholderData, FixPaymentMethod, 0},
		{"", "03", billing.DeclineGeneric, FixPaymentMethod, 0},
		{"", "40", billing.DeclineGeneric, FixPaymentMethod, 0},
		{"", "41", billing.DeclineGeneric, FixPaymentMethod, 0},
		{"", "27", billing.DeclineGeneric, Retry, 4 * 24 * time.Hour},
		{"try_again_later", "02", billing.DeclineGeneric, Retry, 0},
	} {
		r := ClassifyEvidence(Evidence{Rail: "stripe", Code: "generic_decline", Advice: tc.advice, NetworkAdvice: tc.network})
		if r.Reason != tc.reason || r.Action != tc.action || r.RetryAfter != tc.after {
			t.Errorf("advice %q/%q: %s/%s/%s, want %s/%s/%s", tc.advice, tc.network, r.Reason, r.Action, r.RetryAfter, tc.reason, tc.action, tc.after)
		}
	}
}

func TestVisaCategory(t *testing.T) {
	for code, want := range map[string]int{"04": 1, "R0": 1, "51": 2, "59": 2, "54": 3, "N7": 3, "05": 4, "": 0} {
		if got := VisaCategory(code); got != want {
			t.Errorf("VisaCategory(%q) = %d, want %d", code, got, want)
		}
	}
}
