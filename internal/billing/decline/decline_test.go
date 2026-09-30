package decline

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every table row names a reason with a policy, and every reason is used.
func TestTablesAreComplete(t *testing.T) {
	used := map[Reason]bool{UnknownReason: true}
	for rail, table := range rails {
		for code, reason := range table.codes {
			_, ok := reasons[reason]
			require.True(t, ok, "%s %s: reason %q has no policy", rail, code, reason)
			used[reason] = true
		}
	}
	for reason, spec := range reasons {
		require.True(t, used[reason], "reason %q is in no rail table", reason)
		_, ok := customerCopy[spec.customer]
		require.True(t, ok, "reason %q: buyer reason %q has no copy", reason, spec.customer)
	}
	for code, c := range nmiCodes {
		require.NotEmpty(t, c.localization, "nmi %d", code)
		require.Equal(t, strconv.Itoa(code), canonicalNMI(c.localization))
		require.Equal(t, strconv.Itoa(code), canonicalNMI("nmi_"+c.localization))
		require.Equal(t, strconv.Itoa(code), canonicalNMI("nmi_response_"+strconv.Itoa(code)))
	}
}

// The dunning policy per NMI code (#1108 decision 2) is pinned: a moved row
// changes who keeps being charged or who is cancelled.
func TestNMIActions(t *testing.T) {
	want := map[Action][]int{
		Retry:            {200, 201, 202, 203, 240, 260, 264, 300, 400, 410, 411, 420, 421, 430, 440, 441, 460},
		FixPaymentMethod: {204, 220, 221, 222, 223, 224, 225, 226, 250, 251, 263, 461},
		NonRecoverable:   {252, 253, 261, 262},
	}
	n := 0
	for action, codes := range want {
		for _, code := range codes {
			require.Equal(t, action, Classify("nmi", strconv.Itoa(code)).Action, "nmi %d", code)
			n++
		}
	}
	require.Equal(t, len(nmiCodes)-1, n, "every NMI decline code is pinned")
}

func TestClassifyEvidence(t *testing.T) {
	for _, tc := range []struct {
		e      Evidence
		reason Reason
		cov    Coverage
	}{
		{Evidence{Rail: "nmi", Code: "200", CVV: "N"}, IncorrectCVC, Mapped},
		{Evidence{Rail: "nmi", Code: "nmi_do_not_honor", AVS: "N"}, IncorrectZip, Mapped},
		{Evidence{Rail: "nmi", Code: "200", AVS: "W"}, IncorrectAddress, Mapped},
		{Evidence{Rail: "nmi", Code: "202", AVS: "N"}, InsufficientFunds, Mapped},
		{Evidence{Rail: "nmi", Code: "252", CVV: "N"}, StolenCard, Mapped},
		{Evidence{Rail: "nmi", Code: "300", Text: "Duplicate transaction REFID:1"}, DuplicateTransaction, Mapped},
		{Evidence{Rail: "stripe", Code: "", FallbackCode: "expired_card"}, ExpiredCard, Mapped},
		{Evidence{Rail: "stripe", Code: "generic_decline", CVV: "fail"}, IncorrectCVC, Mapped},
		{Evidence{Rail: "ccbill", Code: "BE-950"}, ProcessingError, Mapped},
		{Evidence{Rail: "nmi", Code: "999"}, UnknownReason, Unmapped},
		{Evidence{Rail: "nmi", Code: ""}, UnknownReason, NoCode},
		{Evidence{Rail: "vaulted_card", Code: "202"}, UnknownReason, NoCode},
	} {
		r := ClassifyEvidence(tc.e)
		require.Equal(t, tc.reason, r.Reason, "%+v", tc.e)
		require.Equal(t, tc.cov, r.Coverage, "%+v", tc.e)
	}
	require.Equal(t, CustomerGeneric, Classify("nmi", "251").PaymentFailure().Reason, "fraud signals stay hidden")
	require.Equal(t, "cvc", ClassifyEvidence(Evidence{Rail: "nmi", Code: "200", CVV: "N"}).PaymentFailure().Field)
}

// Buyers see a fixed taxonomy; fraud signals never reveal themselves.
func TestPaymentFailure(t *testing.T) {
	for _, tc := range []struct {
		e             Evidence
		reason, field string
	}{
		{Evidence{Rail: "stripe", Code: "incorrect_cvc", FallbackCode: "card_declined"}, CustomerIncorrectCVC, "cvc"},
		{Evidence{Rail: "stripe", Code: "expired_card"}, CustomerExpiredCard, "expiry"},
		{Evidence{Rail: "stripe", Code: "incorrect_zip"}, CustomerIncorrectZip, "postal_code"},
		{Evidence{Rail: "stripe", FallbackCode: "processing_error"}, CustomerProcessingError, ""},
		{Evidence{Rail: "stripe", Code: "generic_decline", AVS: "fail"}, CustomerIncorrectZip, "postal_code"},
		{Evidence{Rail: "stripe", Code: "insufficient_funds", AVS: "fail"}, CustomerInsufficientFunds, ""},
		{Evidence{Rail: "stripe", Code: "payment_intent_authentication_failure"}, CustomerAuthenticationRequired, ""},
		{Evidence{Rail: "stripe", Code: "something_new"}, CustomerGeneric, ""},
		{Evidence{Rail: "stripe", Code: "fraudulent", CVV: "fail"}, CustomerGeneric, ""},
		{Evidence{Rail: "nmi", Code: "203"}, CustomerOverLimit, ""},
		{Evidence{Rail: "nmi", Code: "expired_card"}, CustomerExpiredCard, "expiry"},
		{Evidence{Rail: "nmi", Code: "invalid_card_security_code"}, CustomerIncorrectCVC, "cvc"},
		{Evidence{Rail: "nmi", Code: "nmi_response_201"}, CustomerDoNotHonor, ""},
		{Evidence{Rail: "nmi", Code: "transaction_was_declined_by_processor", AVS: "A"}, CustomerIncorrectZip, "postal_code"},
		{Evidence{Rail: "nmi", Code: "201", AVS: "Z"}, CustomerIncorrectAddress, ""},
		{Evidence{Rail: "nmi", Code: "421"}, CustomerTryAgainLater, ""},
		{Evidence{Rail: "nmi", Code: "pick_up_card", CVV: "N"}, CustomerGeneric, ""},
		{Evidence{Rail: "nmi", Code: "nmi_response_253"}, CustomerGeneric, ""},
		{Evidence{Rail: "nmi"}, CustomerGeneric, ""},
	} {
		got := ClassifyEvidence(tc.e).PaymentFailure()
		require.Equal(t, tc.reason, got.Reason, "%+v", tc.e)
		require.Equal(t, tc.field, got.Field, "%+v", tc.e)
		require.NotEmpty(t, got.Message)
	}
	require.Equal(t, Failure(CustomerGeneric), Failure("not_a_reason"))
}
