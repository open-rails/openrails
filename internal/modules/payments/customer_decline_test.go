package payments

import "testing"

func TestCustomerDecline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		detail        DeclineDetail
		reason, field string
	}{
		{"stripe incorrect cvc", DeclineDetail{Rail: "stripe", Code: "incorrect_cvc", FallbackCode: "card_declined"}, DeclineIncorrectCVC, "cvc"},
		{"stripe expired", DeclineDetail{Rail: "stripe", Code: "expired_card"}, DeclineExpiredCard, "expiry"},
		{"stripe insufficient funds", DeclineDetail{Rail: "stripe", Code: "insufficient_funds"}, DeclineInsufficientFunds, ""},
		{"stripe zip", DeclineDetail{Rail: "stripe", Code: "incorrect_zip"}, DeclineIncorrectZip, "postal_code"},
		{"stripe fallback code", DeclineDetail{Rail: "stripe", FallbackCode: "processing_error"}, DeclineProcessingError, ""},
		{"stripe postal check", DeclineDetail{Rail: "stripe", Code: "generic_decline", AVS: "fail"}, DeclineIncorrectZip, "postal_code"},
		{"stripe cvc check", DeclineDetail{Rail: "stripe", Code: "card_declined", CVV: "fail"}, DeclineIncorrectCVC, "cvc"},
		{"stripe authentication", DeclineDetail{Rail: "stripe", Code: "payment_intent_authentication_failure"}, DeclineAuthenticationRequired, ""},
		{"stripe unknown", DeclineDetail{Rail: "stripe", Code: "something_new"}, DeclineGeneric, ""},
		{"stripe stolen masked", DeclineDetail{Rail: "stripe", Code: "stolen_card"}, DeclineGeneric, ""},
		{"stripe fraud masked over cvc check", DeclineDetail{Rail: "stripe", Code: "fraudulent", CVV: "fail"}, DeclineGeneric, ""},
		{"stripe restricted masked", DeclineDetail{Rail: "stripe", Code: "restricted_card"}, DeclineGeneric, ""},
		{"nmi insufficient funds", DeclineDetail{Rail: "nmi", Code: "202"}, DeclineInsufficientFunds, ""},
		{"nmi over limit", DeclineDetail{Rail: "nmi", Code: "203"}, DeclineOverLimit, ""},
		{"nmi expired localization", DeclineDetail{Rail: "nmi", Code: "expired_card"}, DeclineExpiredCard, "expiry"},
		{"nmi invalid expiry", DeclineDetail{Rail: "nmi", Code: "224"}, DeclineInvalidExpiry, "expiry"},
		{"nmi invalid cvv", DeclineDetail{Rail: "nmi", Code: "invalid_card_security_code"}, DeclineIncorrectCVC, "cvc"},
		{"nmi response prefix", DeclineDetail{Rail: "nmi", Code: "nmi_response_201"}, DeclineDoNotHonor, ""},
		{"nmi cvv mismatch on generic", DeclineDetail{Rail: "nmi", Code: "200", CVV: "N"}, DeclineIncorrectCVC, "cvc"},
		{"nmi avs zip mismatch on generic", DeclineDetail{Rail: "nmi", Code: "transaction_was_declined_by_processor", AVS: "A"}, DeclineIncorrectZip, "postal_code"},
		{"nmi avs address mismatch on generic", DeclineDetail{Rail: "nmi", Code: "201", AVS: "Z"}, DeclineIncorrectAddress, ""},
		{"nmi avs ignored on specific decline", DeclineDetail{Rail: "nmi", Code: "202", AVS: "N"}, DeclineInsufficientFunds, ""},
		{"nmi processor error", DeclineDetail{Rail: "nmi", Code: "400"}, DeclineProcessingError, ""},
		{"nmi try later", DeclineDetail{Rail: "nmi", Code: "421"}, DeclineTryAgainLater, ""},
		{"nmi lost masked", DeclineDetail{Rail: "nmi", Code: "251"}, DeclineGeneric, ""},
		{"nmi pickup masked", DeclineDetail{Rail: "nmi", Code: "pick_up_card", CVV: "N"}, DeclineGeneric, ""},
		{"nmi fraudulent masked", DeclineDetail{Rail: "nmi", Code: "nmi_response_253"}, DeclineGeneric, ""},
		{"no code", DeclineDetail{Rail: "nmi"}, DeclineGeneric, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := CustomerDecline(tc.detail)
			if got.Reason != tc.reason || got.Field != tc.field || got.Message == "" {
				t.Fatalf("CustomerDecline(%+v) = %+v, want reason %q field %q", tc.detail, got, tc.reason, tc.field)
			}
		})
	}
	generic := NewPaymentFailure(DeclineGeneric).Message
	for _, fraud := range []string{"lost_card", "stolen_card", "pickup_card", "fraudulent", "restricted_card", "security_violation", "merchant_blacklist"} {
		if got := CustomerDecline(DeclineDetail{Rail: "stripe", Code: fraud}); got.Message != generic {
			t.Fatalf("%s revealed %q", fraud, got.Message)
		}
	}
}
