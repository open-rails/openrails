package payments

import (
	"strconv"
	"strings"

	"github.com/open-rails/openrails"
)

// Customer-facing decline taxonomy. Raw provider codes stay in operator
// evidence; buyers only ever see these reasons and messages.
const (
	DeclineIncorrectCVC           = "incorrect_cvc"
	DeclineIncorrectZip           = "incorrect_zip"
	DeclineIncorrectAddress       = "incorrect_address"
	DeclineIncorrectNumber        = "incorrect_number"
	DeclineExpiredCard            = "expired_card"
	DeclineInvalidExpiry          = "invalid_expiry"
	DeclineInsufficientFunds      = "insufficient_funds"
	DeclineOverLimit              = "over_limit"
	DeclineCardNotSupported       = "card_not_supported"
	DeclineCurrencyNotSupported   = "currency_not_supported"
	DeclineProcessingError        = "processing_error"
	DeclineTryAgainLater          = "try_again_later"
	DeclineAuthenticationRequired = "authentication_required"
	DeclineDoNotHonor             = "do_not_honor"
	DeclineGeneric                = "generic_decline"
)

var declineCopy = map[string]struct{ message, field string }{
	DeclineIncorrectCVC:           {"The card's security code is incorrect.", "cvc"},
	DeclineIncorrectZip:           {"The postal code doesn't match the card.", "postal_code"},
	DeclineIncorrectAddress:       {"The billing address doesn't match the card.", ""},
	DeclineIncorrectNumber:        {"The card number is incorrect.", "number"},
	DeclineExpiredCard:            {"The card has expired.", "expiry"},
	DeclineInvalidExpiry:          {"The card's expiration date is incorrect.", "expiry"},
	DeclineInsufficientFunds:      {"The card has insufficient funds.", ""},
	DeclineOverLimit:              {"The card is over its limit.", ""},
	DeclineCardNotSupported:       {"This card doesn't support this type of purchase.", ""},
	DeclineCurrencyNotSupported:   {"This card doesn't support this currency.", ""},
	DeclineProcessingError:        {"The payment couldn't be processed. Try again.", ""},
	DeclineTryAgainLater:          {"The card issuer is unavailable. Try again later.", ""},
	DeclineAuthenticationRequired: {"The card issuer requires authentication. Try again and complete the verification.", ""},
	DeclineDoNotHonor:             {"Your card was declined. Contact your bank or try a different card.", ""},
	DeclineGeneric:                {"Your card was declined. Contact your bank or try a different card.", ""},
}

// NewPaymentFailure renders a reason; unknown reasons read generic_decline.
func NewPaymentFailure(reason string) openrails.PaymentFailure {
	c, ok := declineCopy[reason]
	if !ok {
		reason, c = DeclineGeneric, declineCopy[DeclineGeneric]
	}
	return openrails.PaymentFailure{Reason: reason, Message: c.message, Field: c.field}
}

var stripeCustomerDeclines = map[string]string{
	"incorrect_cvc": DeclineIncorrectCVC, "invalid_cvc": DeclineIncorrectCVC,
	"incorrect_zip": DeclineIncorrectZip, "incorrect_address": DeclineIncorrectAddress,
	"incorrect_number": DeclineIncorrectNumber, "invalid_number": DeclineIncorrectNumber,
	"expired_card": DeclineExpiredCard, "invalid_expiry_month": DeclineInvalidExpiry, "invalid_expiry_year": DeclineInvalidExpiry,
	"insufficient_funds": DeclineInsufficientFunds, "withdrawal_count_limit_exceeded": DeclineOverLimit, "card_velocity_exceeded": DeclineOverLimit,
	"card_not_supported": DeclineCardNotSupported, "transaction_not_allowed": DeclineCardNotSupported, "invalid_account": DeclineIncorrectNumber,
	"currency_not_supported": DeclineCurrencyNotSupported,
	"processing_error":       DeclineProcessingError, "reenter_transaction": DeclineProcessingError,
	"issuer_not_available": DeclineTryAgainLater, "try_again_later": DeclineTryAgainLater, "approve_with_id": DeclineTryAgainLater,
	"authentication_required": DeclineAuthenticationRequired, "payment_intent_authentication_failure": DeclineAuthenticationRequired,
	"do_not_honor": DeclineDoNotHonor, "call_issuer": DeclineDoNotHonor, "no_action_taken": DeclineDoNotHonor, "not_permitted": DeclineDoNotHonor,
	"revocation_of_all_authorizations": DeclineDoNotHonor, "revocation_of_authorization": DeclineDoNotHonor, "stop_payment_order": DeclineDoNotHonor,
}

var nmiCustomerDeclines = map[int]string{
	200: DeclineGeneric, 201: DeclineDoNotHonor, 202: DeclineInsufficientFunds, 203: DeclineOverLimit,
	204: DeclineCardNotSupported, 220: DeclineIncorrectNumber, 221: DeclineIncorrectNumber, 222: DeclineIncorrectNumber,
	223: DeclineExpiredCard, 224: DeclineInvalidExpiry, 225: DeclineIncorrectCVC, 226: DeclineGeneric,
	240: DeclineDoNotHonor, 260: DeclineDoNotHonor, 261: DeclineDoNotHonor, 262: DeclineDoNotHonor, 263: DeclineDoNotHonor,
	264: DeclineTryAgainLater, 300: DeclineProcessingError, 400: DeclineProcessingError, 410: DeclineProcessingError,
	411: DeclineProcessingError, 420: DeclineTryAgainLater, 421: DeclineTryAgainLater, 430: DeclineProcessingError,
	440: DeclineProcessingError, 441: DeclineProcessingError, 460: DeclineProcessingError, 461: DeclineCardNotSupported,
}

// Fraud-related codes never reveal their reason to the buyer.
var fraudDeclines = map[string]bool{
	"lost_card": true, "stolen_card": true, "pickup_card": true, "fraudulent": true, "restricted_card": true,
	"security_violation": true, "merchant_blacklist": true, "250": true, "251": true, "252": true, "253": true,
	"pick_up_card": true, "fraudulent_card": true,
}

// DeclineDetail is the raw provider evidence of one definite decline.
type DeclineDetail struct {
	Rail string
	// Code is the provider code: Stripe decline_code (else error code), or the
	// NMI numeric response_code / localization id.
	Code string
	// FallbackCode is Stripe's error code when Code is its decline_code.
	FallbackCode string
	// AVS / CVV are NMI's avsresponse / cvvresponse, or Stripe's
	// address_postal_code_check / cvc_check.
	AVS, CVV string
}

// CustomerDecline maps provider evidence onto the customer taxonomy.
func CustomerDecline(d DeclineDetail) openrails.PaymentFailure {
	code := strings.ToLower(strings.TrimSpace(d.Code))
	fallback := strings.ToLower(strings.TrimSpace(d.FallbackCode))
	if fraudDeclines[code] || fraudDeclines[fallback] || fraudDeclines[strings.TrimPrefix(code, "nmi_response_")] {
		return NewPaymentFailure(DeclineGeneric)
	}
	reason := ""
	switch strings.ToLower(strings.TrimSpace(d.Rail)) {
	case "stripe":
		reason = stripeCustomerDeclines[code]
		if reason == "" {
			reason = stripeCustomerDeclines[fallback]
		}
		if strings.EqualFold(d.CVV, "fail") {
			reason = DeclineIncorrectCVC
		} else if (reason == "" || reason == DeclineGeneric || reason == DeclineDoNotHonor) && strings.EqualFold(d.AVS, "fail") {
			reason = DeclineIncorrectZip
		}
	case "nmi":
		n, err := strconv.Atoi(strings.TrimPrefix(code, "nmi_response_"))
		if err == nil {
			reason = nmiCustomerDeclines[n]
		} else if n, ok := nmiLocalizationCodes[strings.TrimPrefix(code, "nmi_")]; ok {
			reason = nmiCustomerDeclines[n]
		}
		generic := reason == "" || reason == DeclineGeneric || reason == DeclineDoNotHonor || reason == DeclineIncorrectNumber
		switch {
		case strings.EqualFold(d.CVV, "N"):
			reason = DeclineIncorrectCVC
		case generic && nmiAVSZipMismatch(d.AVS):
			reason = DeclineIncorrectZip
		case generic && nmiAVSAddressMismatch(d.AVS):
			reason = DeclineIncorrectAddress
		}
	}
	return NewPaymentFailure(reason)
}

// nmiLocalizationCodes resolves NMI's localization ids (the recorded
// failure_code form) back to the gateway response code.
var nmiLocalizationCodes = map[string]int{
	"transaction_was_declined_by_processor": 200, "do_not_honor": 201, "insufficient_funds": 202, "over_limit": 203,
	"transaction_not_allowed": 204, "incorrect_payment_information": 220, "no_such_card_issuer": 221,
	"no_card_number_on_file_with_issuer": 222, "expired_card": 223, "invalid_expiration_date": 224,
	"invalid_card_security_code": 225, "invalid_pin": 226, "call_issuer_for_further_information": 240,
	"pick_up_card": 250, "lost_card": 251, "stolen_card": 252, "fraudulent_card": 253,
	"declined_with_further_instructions_available_see_response_text": 260, "declined_stop_all_recurring_payments": 261,
	"declined_stop_this_recurring_program": 262, "declined_update_cardholder_data_available": 263,
	"declined_retry_in_a_few_days": 264, "transaction_was_rejected_by_gateway": 300,
	"transaction_error_returned_by_processor": 400, "invalid_merchant_configuration": 410,
	"merchant_account_is_inactive": 411, "communication_error": 420, "communication_error_with_issuer": 421,
	"duplicate_transaction_at_rail": 430, "rail_format_error": 440, "invalid_transaction_information": 441,
	"rail_feature_not_available": 460, "unsupported_card_type": 461,
}

// NMI AVS codes: postal code did not match (address may have).
func nmiAVSZipMismatch(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "A", "B", "N", "O":
		return true
	}
	return false
}

// NMI AVS codes: postal code matched, street address did not.
func nmiAVSAddressMismatch(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "W", "Z", "P", "L":
		return true
	}
	return false
}
