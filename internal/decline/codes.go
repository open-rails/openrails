package decline

import (
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
)

type railTable struct {
	codes     map[string]billing.DeclineReason
	canonical func(code string) string
}

var rails = map[string]railTable{
	"nmi":    {codes: nmiReasons(), canonical: canonicalNMI},
	"stripe": {codes: stripeCodes, canonical: lower},
	"ccbill": {codes: ccbillCodes(), canonical: canonicalCCBill},
	"solana": {codes: solanaCodes, canonical: lower},
}

// nmiCode is one published NMI response_code.
type nmiCode struct {
	reason       billing.DeclineReason
	localization string
	message      string
}

// nmiCodes is NMI's published response_code table
// (docs.nmi.com/reference/response-codes).
var nmiCodes = map[int]nmiCode{
	100: {"", "transaction_was_approved", "Transaction was approved."},
	200: {billing.DeclineGeneric, "transaction_was_declined_by_processor", "Transaction was declined by processor."},
	201: {billing.DeclineDoNotHonor, "do_not_honor", "Do not honor."},
	202: {billing.DeclineInsufficientFunds, "insufficient_funds", "Insufficient funds."},
	203: {billing.DeclineOverLimit, "over_limit", "Over limit."},
	204: {billing.DeclineTransactionNotAllowed, "transaction_not_allowed", "Transaction not allowed."},
	220: {billing.DeclineIncorrectNumber, "incorrect_payment_information", "Incorrect payment information."},
	221: {billing.DeclineNoSuchIssuer, "no_such_card_issuer", "No such card issuer."},
	222: {billing.DeclineInvalidAccount, "no_card_number_on_file_with_issuer", "No card number on file with issuer."},
	223: {billing.DeclineExpiredCard, "expired_card", "Expired card."},
	224: {billing.DeclineInvalidExpiry, "invalid_expiration_date", "Invalid expiration date."},
	225: {billing.DeclineIncorrectCVC, "invalid_card_security_code", "Invalid card security code."},
	226: {billing.DeclineInvalidPIN, "invalid_pin", "Invalid PIN."},
	240: {billing.DeclineCallIssuer, "call_issuer_for_further_information", "Call issuer for further information."},
	250: {billing.DeclinePickupCard, "pick_up_card", "Pick up card."},
	251: {billing.DeclineLostCard, "lost_card", "Lost card."},
	252: {billing.DeclineStolenCard, "stolen_card", "Stolen card."},
	253: {billing.DeclineFraudulent, "fraudulent_card", "Fraudulent card."},
	260: {billing.DeclineGeneric, "declined_with_further_instructions_available_see_response_text", "Declined with further instructions available. (See response text)"},
	261: {billing.DeclineStopRecurring, "declined_stop_all_recurring_payments", "Declined-Stop all recurring payments."},
	262: {billing.DeclineStopRecurring, "declined_stop_this_recurring_program", "Declined-Stop this recurring program."},
	263: {billing.DeclineUpdateCardholderData, "declined_update_cardholder_data_available", "Declined-Update cardholder data available."},
	264: {billing.DeclineRetryLater, "declined_retry_in_a_few_days", "Declined-Retry in a few days."},
	300: {billing.DeclineGatewayRejected, "transaction_was_rejected_by_gateway", "Transaction was rejected by gateway."},
	400: {billing.DeclineProcessingError, "transaction_error_returned_by_processor", "Transaction error returned by processor."},
	410: {billing.DeclineMerchantConfig, "invalid_merchant_configuration", "Invalid merchant configuration."},
	411: {billing.DeclineMerchantConfig, "merchant_account_is_inactive", "Merchant account is inactive."},
	420: {billing.DeclineCommunicationError, "communication_error", "Communication error."},
	421: {billing.DeclineCommunicationError, "communication_error_with_issuer", "Communication error with issuer."},
	430: {billing.DeclineDuplicateTransaction, "duplicate_transaction_at_processor", "Duplicate transaction at processor."},
	440: {billing.DeclineInvalidRequest, "processor_format_error", "Processor format error."},
	441: {billing.DeclineInvalidRequest, "invalid_transaction_information", "Invalid transaction information."},
	460: {billing.DeclineInvalidRequest, "processor_feature_not_available", "Processor feature not available."},
	461: {billing.DeclineCardNotSupported, "unsupported_card_type", "Unsupported card type."},
}

var nmiByLocalization = func() map[string]int {
	m := make(map[string]int, len(nmiCodes))
	for code, c := range nmiCodes {
		m[c.localization] = code
	}
	return m
}()

func nmiReasons() map[string]billing.DeclineReason {
	m := make(map[string]billing.DeclineReason, len(nmiCodes))
	for code, c := range nmiCodes {
		if c.reason != "" {
			m[strconv.Itoa(code)] = c.reason
		}
	}
	return m
}

// canonicalNMI folds every recorded form of an NMI code onto its number:
// 225, nmi_response_225, invalid_card_security_code and
// nmi_invalid_card_security_code are one code.
func canonicalNMI(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if n, err := strconv.Atoi(strings.TrimPrefix(code, "nmi_response_")); err == nil {
		return strconv.Itoa(n)
	}
	if n, ok := nmiByLocalization[code]; ok {
		return strconv.Itoa(n)
	}
	if n, ok := nmiByLocalization[strings.TrimPrefix(code, "nmi_")]; ok {
		return strconv.Itoa(n)
	}
	return code
}

// NMILocalizationID is NMI's localization id for a response code ("" if none).
func NMILocalizationID(code int) string { return nmiCodes[code].localization }

// NMIMessage is NMI's published text for a response code ("" if none).
func NMIMessage(code int) string { return nmiCodes[code].message }

var stripeCodes = map[string]billing.DeclineReason{
	"generic_decline":                       billing.DeclineGeneric,
	"card_declined":                         billing.DeclineGeneric,
	"do_not_honor":                          billing.DeclineDoNotHonor,
	"no_action_taken":                       billing.DeclineDoNotHonor,
	"call_issuer":                           billing.DeclineCallIssuer,
	"insufficient_funds":                    billing.DeclineInsufficientFunds,
	"withdrawal_count_limit_exceeded":       billing.DeclineOverLimit,
	"card_velocity_exceeded":                billing.DeclineOverLimit,
	"try_again_later":                       billing.DeclineTryAgainLater,
	"reenter_transaction":                   billing.DeclineTryAgainLater,
	"approve_with_id":                       billing.DeclineTryAgainLater,
	"security_violation":                    billing.DeclineSecurityViolation,
	"restricted_card":                       billing.DeclineRestrictedCard,
	"incorrect_number":                      billing.DeclineIncorrectNumber,
	"invalid_number":                        billing.DeclineIncorrectNumber,
	"invalid_account":                       billing.DeclineInvalidAccount,
	"no_account":                            billing.DeclineInvalidAccount,
	"expired_card":                          billing.DeclineExpiredCard,
	"invalid_expiry_month":                  billing.DeclineInvalidExpiry,
	"invalid_expiry_year":                   billing.DeclineInvalidExpiry,
	"incorrect_cvc":                         billing.DeclineIncorrectCVC,
	"invalid_cvc":                           billing.DeclineIncorrectCVC,
	"incorrect_zip":                         billing.DeclineIncorrectZip,
	"incorrect_address":                     billing.DeclineIncorrectAddress,
	"incorrect_pin":                         billing.DeclineInvalidPIN,
	"invalid_pin":                           billing.DeclineInvalidPIN,
	"pin_try_exceeded":                      billing.DeclineInvalidPIN,
	"offline_pin_required":                  billing.DeclineInvalidPIN,
	"online_or_offline_pin_required":        billing.DeclineInvalidPIN,
	"transaction_not_allowed":               billing.DeclineTransactionNotAllowed,
	"not_permitted":                         billing.DeclineTransactionNotAllowed,
	"card_not_supported":                    billing.DeclineCardNotSupported,
	"currency_not_supported":                billing.DeclineCurrencyNotSupported,
	"authentication_required":               billing.DeclineAuthenticationRequired,
	"payment_intent_authentication_failure": billing.DeclineAuthenticationRequired,
	"pickup_card":                           billing.DeclinePickupCard,
	"lost_card":                             billing.DeclineLostCard,
	"stolen_card":                           billing.DeclineStolenCard,
	"fraudulent":                            billing.DeclineFraudulent,
	"stop_payment_order":                    billing.DeclineStopRecurring,
	"revocation_of_authorization":           billing.DeclineStopRecurring,
	"revocation_of_all_authorizations":      billing.DeclineStopRecurring,
	"merchant_blacklist":                    billing.DeclineBlockedByPSP,
	"processing_error":                      billing.DeclineProcessingError,
	"issuer_not_available":                  billing.DeclineIssuerUnavailable,
	"duplicate_transaction":                 billing.DeclineDuplicateTransaction,
}

// ccbillCodes is CCBill's own BE-nnn vocabulary
// (ccbill.com/kb/list-of-credit-card-declined-codes). BE-900..999 are system
// errors.
func ccbillCodes() map[string]billing.DeclineReason {
	m := map[string]billing.DeclineReason{
		"be101": billing.DeclineMerchantConfig,
		"be102": billing.DeclinePickupCard,
		"be103": billing.DeclineDoNotHonor,
		"be105": billing.DeclineInvalidRequest,
		"be107": billing.DeclineIncorrectNumber,
		"be112": billing.DeclineInvalidAccount,
		"be113": billing.DeclineInsufficientFunds,
		"be114": billing.DeclineExpiredCard,
		"be116": billing.DeclineTransactionNotAllowed,
		"be119": billing.DeclineOverLimit,
		"be130": billing.DeclineInvalidRequest,
		"be132": billing.DeclineBlockedByPSP, // card blocked by CCBill
		"be146": billing.DeclineBlockedByPSP, // blocked country
	}
	for n := 900; n <= 999; n++ {
		m["be"+strconv.Itoa(n)] = billing.DeclineProcessingError
	}
	return m
}

// canonicalCCBill folds BE-114, be-114 and BE114 onto one key.
func canonicalCCBill(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(code) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// solanaCodes are the Solana crank's failure codes. A revoked delegate is the
// crank's own terminal case; the rest are retried or never reach dunning.
var solanaCodes = map[string]billing.DeclineReason{
	"insufficient_funds":                   billing.DeclineInsufficientFunds,
	"do_not_honor":                         billing.DeclineDoNotHonor,
	"generic_decline":                      billing.DeclineGeneric,
	"declined_update_cardholder_data":      billing.DeclineUpdateCardholderData,
	"declined_stop_all_recurring_payments": billing.DeclineStopRecurring,
	"declined_stop_this_recurring_program": billing.DeclineStopRecurring,
	"duplicate_transaction":                billing.DeclineDuplicateTransaction,
	"merchant_configuration_error":         billing.DeclineMerchantConfig,
	"communication_error":                  billing.DeclineCommunicationError,
	"processing_error":                     billing.DeclineProcessingError,
}

func lower(code string) string { return strings.ToLower(strings.TrimSpace(code)) }

// cvvMismatch: NMI cvvresponse N, Stripe cvc_check fail.
func cvvMismatch(rail, cvv string) bool {
	cvv = strings.TrimSpace(cvv)
	if rail == "stripe" {
		return strings.EqualFold(cvv, "fail")
	}
	return strings.EqualFold(cvv, "N")
}

// zipMismatch: NMI AVS letters where the postal code did not match, Stripe
// address_postal_code_check fail.
func zipMismatch(rail, avs string) bool {
	avs = strings.ToUpper(strings.TrimSpace(avs))
	if rail == "stripe" {
		return avs == "FAIL"
	}
	switch avs {
	case "A", "B", "N", "O":
		return true
	}
	return false
}

// addressMismatch: NMI AVS letters where the postal code matched and the
// street did not.
func addressMismatch(rail, avs string) bool {
	if rail == "stripe" {
		return false
	}
	switch strings.ToUpper(strings.TrimSpace(avs)) {
	case "W", "Z", "P", "L":
		return true
	}
	return false
}
