package decline

import (
	"maps"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Every table row names a reason with a policy, and every reason is used.
func TestTablesAreComplete(t *testing.T) {
	used := map[billing.DeclineReason]bool{billing.DeclineUnknown: true}
	for rail, table := range rails {
		for code, reason := range table.codes {
			_, ok := reasons[reason]
			require.True(t, ok, "%s %s: reason %q has no policy", rail, code, reason)
			used[reason] = true
		}
	}
	require.ElementsMatch(t, billing.DeclineReasons(), slices.Collect(maps.Keys(reasons)), "every public reason has a policy")
	for reason := range reasons {
		require.True(t, used[reason], "reason %q is in no rail table", reason)
	}
	for code, c := range nmiCodes {
		require.NotEmpty(t, c.localization, "nmi %d", code)
		require.Equal(t, strconv.Itoa(code), canonicalNMI(c.localization))
		require.Equal(t, strconv.Itoa(code), canonicalNMI("nmi_"+c.localization))
		require.Equal(t, strconv.Itoa(code), canonicalNMI("nmi_response_"+strconv.Itoa(code)))
	}
}

// The dunning policy per NMI code is pinned: a moved row changes who keeps
// being charged or who is canceled.
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
		reason billing.DeclineReason
		cov    Coverage
	}{
		{Evidence{Rail: "nmi", Code: "200", CVV: "N"}, billing.DeclineIncorrectCVC, Mapped},
		{Evidence{Rail: "nmi", Code: "nmi_do_not_honor", AVS: "N"}, billing.DeclineIncorrectZip, Mapped},
		{Evidence{Rail: "nmi", Code: "200", AVS: "W"}, billing.DeclineIncorrectAddress, Mapped},
		{Evidence{Rail: "nmi", Code: "202", AVS: "N"}, billing.DeclineInsufficientFunds, Mapped},
		{Evidence{Rail: "nmi", Code: "252", CVV: "N"}, billing.DeclineStolenCard, Mapped},
		{Evidence{Rail: "nmi", Code: "300", Text: "Duplicate transaction REFID:1"}, billing.DeclineDuplicateTransaction, Mapped},
		{Evidence{Rail: "stripe", Code: "", FallbackCode: "expired_card"}, billing.DeclineExpiredCard, Mapped},
		{Evidence{Rail: "stripe", Code: "generic_decline", CVV: "fail"}, billing.DeclineIncorrectCVC, Mapped},
		{Evidence{Rail: "ccbill", Code: "BE-950"}, billing.DeclineProcessingError, Mapped},
		{Evidence{Rail: "nmi", Code: "999"}, billing.DeclineUnknown, Unmapped},
		{Evidence{Rail: "nmi", Code: ""}, billing.DeclineUnknown, NoCode},
		{Evidence{Rail: "vaulted_card", Code: "202"}, billing.DeclineUnknown, NoCode},
	} {
		r := ClassifyEvidence(tc.e)
		require.Equal(t, tc.reason, r.Reason, "%+v", tc.e)
		require.Equal(t, tc.cov, r.Coverage, "%+v", tc.e)
	}
	require.Equal(t, "generic_decline", Classify("nmi", "251").Reason.Failure().Reason, "fraud signals stay hidden")
	require.Equal(t, "cvc", ClassifyEvidence(Evidence{Rail: "nmi", Code: "200", CVV: "N"}).Reason.Failure().Field)
}

// Buyers see a fixed taxonomy; fraud signals never reveal themselves.
func TestPaymentFailure(t *testing.T) {
	for _, tc := range []struct {
		e             Evidence
		reason, field string
	}{
		{Evidence{Rail: "stripe", Code: "incorrect_cvc", FallbackCode: "card_declined"}, "incorrect_cvc", "cvc"},
		{Evidence{Rail: "stripe", Code: "expired_card"}, "expired_card", "expiry"},
		{Evidence{Rail: "stripe", Code: "incorrect_zip"}, "incorrect_zip", "postal_code"},
		{Evidence{Rail: "stripe", FallbackCode: "processing_error"}, "processing_error", ""},
		{Evidence{Rail: "stripe", Code: "generic_decline", AVS: "fail"}, "incorrect_zip", "postal_code"},
		{Evidence{Rail: "stripe", Code: "insufficient_funds", AVS: "fail"}, "insufficient_funds", ""},
		{Evidence{Rail: "stripe", Code: "payment_intent_authentication_failure"}, "authentication_required", ""},
		{Evidence{Rail: "stripe", Code: "something_new"}, "generic_decline", ""},
		{Evidence{Rail: "stripe", Code: "fraudulent", CVV: "fail"}, "generic_decline", ""},
		{Evidence{Rail: "nmi", Code: "203"}, "over_limit", ""},
		{Evidence{Rail: "nmi", Code: "expired_card"}, "expired_card", "expiry"},
		{Evidence{Rail: "nmi", Code: "invalid_card_security_code"}, "incorrect_cvc", "cvc"},
		{Evidence{Rail: "nmi", Code: "nmi_response_201"}, "do_not_honor", ""},
		{Evidence{Rail: "nmi", Code: "transaction_was_declined_by_processor", AVS: "A"}, "incorrect_zip", "postal_code"},
		{Evidence{Rail: "nmi", Code: "201", AVS: "Z"}, "incorrect_address", ""},
		{Evidence{Rail: "nmi", Code: "421"}, "try_again_later", ""},
		{Evidence{Rail: "nmi", Code: "pick_up_card", CVV: "N"}, "generic_decline", ""},
		{Evidence{Rail: "nmi", Code: "nmi_response_253"}, "generic_decline", ""},
		{Evidence{Rail: "nmi"}, "generic_decline", ""},
	} {
		got := ClassifyEvidence(tc.e).Reason.Failure()
		require.Equal(t, tc.reason, got.Reason, "%+v", tc.e)
		require.Equal(t, tc.field, got.Field, "%+v", tc.e)
		require.NotEmpty(t, got.Message)
	}
	require.Equal(t, billing.DeclineGeneric.Failure(), billing.DeclineReason("not_a_reason").Failure())
}
