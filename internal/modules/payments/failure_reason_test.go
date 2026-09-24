package payments

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// FAB-1 / #733: raw codes map deterministically; anything unmapped reads "unknown", never a guess.
func TestNormalizeFailureReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ rail, code, want string }{
		{"nmi", "202", FailureInsufficientFunds},
		{"nmi", "223", FailureExpiredCard},
		{"nmi", "253", FailureFraudSuspected},
		{"nmi", "261", FailureStopRecurring},
		{"nmi", "410", FailureConfigError},
		{"nmi", "430", FailureDuplicateTransaction},
		{"nmi", "999", FailureUnknown},
		{"nmi", "insufficient_funds", FailureInsufficientFunds},
		{"nmi", "declined_stop_all_recurring_payments", FailureStopRecurring},
		{"nmi", "something_new", FailureUnknown},
		{"stripe", "generic_decline", FailureGenericDecline},
		{"stripe", "incorrect_zip", FailureCVVAVS},
		{"stripe", "revocation_of_authorization", FailureStopRecurring},
		{"stripe", "202", FailureUnknown},
		{"solana", "merchant_configuration_error", FailureConfigError},
		{"solana", "incorrect_cvc", FailureUnknown},
		{"mobius", "202", FailureUnknown}, // a PSP key, not a rail
		{"nmi", "", FailureUnknown},
	} {
		require.Equal(t, tc.want, NormalizeFailureReason(tc.rail, tc.code), "%s/%s", tc.rail, tc.code)
	}
}

// or#870: CCBill's own BE-nnn vocabulary, pinned in full; a re-categorization is a policy change.
func TestCCBillFailureReasons(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"BE-101": FailureConfigError, // ours, never the customer's
		"BE-102": FailureFraudSuspected,
		"BE-103": FailureCardDeclined,
		"BE-105": FailureProcessorError,
		"BE-107": FailureCardDeclined,
		"BE-112": FailureCardDeclined,
		"BE-113": FailureInsufficientFunds,
		"BE-114": FailureExpiredCard,
		"BE-116": FailureCardDeclined,
		"BE-119": FailureCardDeclined,
		"BE-130": FailureProcessorError,
		"BE-132": FailureCardDeclined,
		"BE-146": FailureCardDeclined,
	}
	for code, reason := range want {
		require.Equal(t, reason, NormalizeFailureReason("ccbill", code), code)
	}
	require.Len(t, ccbillFailureReasons, len(want), "a new row is a policy change; update the pin")

	for _, shape := range []string{"be-114", "BE114", "be114", " be-114 "} {
		require.Equal(t, FailureExpiredCard, NormalizeFailureReason("ccbill", shape), shape)
	}
	for _, code := range []string{"BE-900", "BE-950", "BE-999"} {
		require.Equal(t, FailureProcessorError, NormalizeFailureReason("ccbill", code), code)
	}
	for _, code := range []string{"BE-899", "BE-1000", "BE-9", "BE-777", "declined", "202", ""} {
		require.Equal(t, FailureUnknown, NormalizeFailureReason("ccbill", code), code)
	}
}

// or#879: the token_type stamp needs rail AND custody; unstated custody stamps nothing.
func TestDefaultTokenType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ rail, custodian, want string }{
		{"nmi", models.CustodianPSP, charge.TokenTypePSPToken},
		{"nmi", models.CustodianBasisTheory, charge.TokenTypePANViaProxy},
		{" NMI ", models.CustodianHyperSwitch, charge.TokenTypePANViaProxy},
		{"nmi", "", ""},
		{"stripe", models.CustodianPSP, charge.TokenTypePSPToken},
		{"stripe", models.CustodianBasisTheory, ""},
		{"ccbill", models.CustodianPSP, ""},
		{"solana", "", ""},
		{"vaulted_card", models.CustodianBasisTheory, ""},
		{"mobius", models.CustodianPSP, ""},
	} {
		require.Equal(t, tc.want, DefaultTokenType(tc.rail, tc.custodian), "%s/%s", tc.rail, tc.custodian)
	}
}

// Buyers see a fixed taxonomy; fraud-related codes never reveal themselves.
func TestCustomerDecline(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		detail        DeclineDetail
		reason, field string
	}{
		{"stripe cvc over fallback", DeclineDetail{Rail: "stripe", Code: "incorrect_cvc", FallbackCode: "card_declined"}, DeclineIncorrectCVC, "cvc"},
		{"stripe expired", DeclineDetail{Rail: "stripe", Code: "expired_card"}, DeclineExpiredCard, "expiry"},
		{"stripe insufficient", DeclineDetail{Rail: "stripe", Code: "insufficient_funds"}, DeclineInsufficientFunds, ""},
		{"stripe zip", DeclineDetail{Rail: "stripe", Code: "incorrect_zip"}, DeclineIncorrectZip, "postal_code"},
		{"stripe fallback", DeclineDetail{Rail: "stripe", FallbackCode: "processing_error"}, DeclineProcessingError, ""},
		{"stripe postal check on generic", DeclineDetail{Rail: "stripe", Code: "generic_decline", AVS: "fail"}, DeclineIncorrectZip, "postal_code"},
		{"stripe postal check ignored on specific", DeclineDetail{Rail: "stripe", Code: "insufficient_funds", AVS: "fail"}, DeclineInsufficientFunds, ""},
		{"stripe cvc check", DeclineDetail{Rail: "stripe", Code: "card_declined", CVV: "FAIL"}, DeclineIncorrectCVC, "cvc"},
		{"stripe authentication", DeclineDetail{Rail: "stripe", Code: "payment_intent_authentication_failure"}, DeclineAuthenticationRequired, ""},
		{"stripe unknown", DeclineDetail{Rail: "stripe", Code: "something_new"}, DeclineGeneric, ""},
		{"stripe fraud masks cvc check", DeclineDetail{Rail: "stripe", Code: "fraudulent", CVV: "fail"}, DeclineGeneric, ""},
		{"stripe fraud fallback masked", DeclineDetail{Rail: "stripe", Code: "card_declined", FallbackCode: "stolen_card"}, DeclineGeneric, ""},
		{"nmi insufficient", DeclineDetail{Rail: "nmi", Code: "202"}, DeclineInsufficientFunds, ""},
		{"nmi over limit", DeclineDetail{Rail: "nmi", Code: "203"}, DeclineOverLimit, ""},
		{"nmi localization expired", DeclineDetail{Rail: "nmi", Code: "expired_card"}, DeclineExpiredCard, "expiry"},
		{"nmi invalid expiry", DeclineDetail{Rail: "nmi", Code: "224"}, DeclineInvalidExpiry, "expiry"},
		{"nmi invalid cvv", DeclineDetail{Rail: "nmi", Code: "invalid_card_security_code"}, DeclineIncorrectCVC, "cvc"},
		{"nmi response prefix", DeclineDetail{Rail: "nmi", Code: "nmi_response_201"}, DeclineDoNotHonor, ""},
		{"nmi cvv mismatch", DeclineDetail{Rail: "nmi", Code: "200", CVV: "n"}, DeclineIncorrectCVC, "cvc"},
		{"nmi avs zip on generic", DeclineDetail{Rail: "nmi", Code: "transaction_was_declined_by_processor", AVS: "A"}, DeclineIncorrectZip, "postal_code"},
		{"nmi avs address on generic", DeclineDetail{Rail: "nmi", Code: "201", AVS: "Z"}, DeclineIncorrectAddress, ""},
		{"nmi avs ignored on specific", DeclineDetail{Rail: "nmi", Code: "202", AVS: "N"}, DeclineInsufficientFunds, ""},
		{"nmi processor error", DeclineDetail{Rail: "nmi", Code: "400"}, DeclineProcessingError, ""},
		{"nmi try later", DeclineDetail{Rail: "nmi", Code: "421"}, DeclineTryAgainLater, ""},
		{"nmi lost masked", DeclineDetail{Rail: "nmi", Code: "251"}, DeclineGeneric, ""},
		{"nmi pickup masks cvv", DeclineDetail{Rail: "nmi", Code: "pick_up_card", CVV: "N"}, DeclineGeneric, ""},
		{"nmi prefixed fraud masked", DeclineDetail{Rail: "nmi", Code: "nmi_response_253"}, DeclineGeneric, ""},
		{"no code", DeclineDetail{Rail: "nmi"}, DeclineGeneric, ""},
		{"unknown rail", DeclineDetail{Rail: "ccbill", Code: "BE-113"}, DeclineGeneric, ""},
	} {
		got := CustomerDecline(tc.detail)
		require.Equal(t, tc.reason, got.Reason, tc.name)
		require.Equal(t, tc.field, got.Field, tc.name)
		require.NotEmpty(t, got.Message, tc.name)
	}

	generic := NewPaymentFailure(DeclineGeneric)
	require.Equal(t, generic, NewPaymentFailure("not_a_reason"), "unknown reasons render generic")
	for _, fraud := range []string{"lost_card", "stolen_card", "pickup_card", "fraudulent", "restricted_card", "security_violation", "merchant_blacklist"} {
		require.Equal(t, generic.Message, CustomerDecline(DeclineDetail{Rail: "stripe", Code: fraud}).Message, fraud)
	}
	for reason := range declineCopy {
		require.NotEmpty(t, NewPaymentFailure(reason).Message, reason)
	}
}
