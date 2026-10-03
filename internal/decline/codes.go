package decline

import (
	"strconv"
	"strings"

	"github.com/open-rails/openrails"
)

type railTable struct {
	codes     map[string]openrails.DeclineReason
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
	reason       openrails.DeclineReason
	localization string
	message      string
}

// nmiCodes is NMI's published response_code table
// (docs.nmi.com/reference/response-codes).
var nmiCodes = map[int]nmiCode{
	100: {"", "transaction_was_approved", "Transaction was approved."},
	200: {openrails.DeclineGeneric, "transaction_was_declined_by_processor", "Transaction was declined by processor."},
	201: {openrails.DeclineDoNotHonor, "do_not_honor", "Do not honor."},
	202: {openrails.DeclineInsufficientFunds, "insufficient_funds", "Insufficient funds."},
	203: {openrails.DeclineOverLimit, "over_limit", "Over limit."},
	204: {openrails.DeclineTransactionNotAllowed, "transaction_not_allowed", "Transaction not allowed."},
	220: {openrails.DeclineIncorrectNumber, "incorrect_payment_information", "Incorrect payment information."},
	221: {openrails.DeclineNoSuchIssuer, "no_such_card_issuer", "No such card issuer."},
	222: {openrails.DeclineInvalidAccount, "no_card_number_on_file_with_issuer", "No card number on file with issuer."},
	223: {openrails.DeclineExpiredCard, "expired_card", "Expired card."},
	224: {openrails.DeclineInvalidExpiry, "invalid_expiration_date", "Invalid expiration date."},
	225: {openrails.DeclineIncorrectCVC, "invalid_card_security_code", "Invalid card security code."},
	226: {openrails.DeclineInvalidPIN, "invalid_pin", "Invalid PIN."},
	240: {openrails.DeclineCallIssuer, "call_issuer_for_further_information", "Call issuer for further information."},
	250: {openrails.DeclinePickupCard, "pick_up_card", "Pick up card."},
	251: {openrails.DeclineLostCard, "lost_card", "Lost card."},
	252: {openrails.DeclineStolenCard, "stolen_card", "Stolen card."},
	253: {openrails.DeclineFraudulent, "fraudulent_card", "Fraudulent card."},
	260: {openrails.DeclineGeneric, "declined_with_further_instructions_available_see_response_text", "Declined with further instructions available. (See response text)"},
	261: {openrails.DeclineStopRecurring, "declined_stop_all_recurring_payments", "Declined-Stop all recurring payments."},
	262: {openrails.DeclineStopRecurring, "declined_stop_this_recurring_program", "Declined-Stop this recurring program."},
	263: {openrails.DeclineUpdateCardholderData, "declined_update_cardholder_data_available", "Declined-Update cardholder data available."},
	264: {openrails.DeclineRetryLater, "declined_retry_in_a_few_days", "Declined-Retry in a few days."},
	300: {openrails.DeclineGatewayRejected, "transaction_was_rejected_by_gateway", "Transaction was rejected by gateway."},
	400: {openrails.DeclineProcessingError, "transaction_error_returned_by_processor", "Transaction error returned by processor."},
	410: {openrails.DeclineMerchantConfig, "invalid_merchant_configuration", "Invalid merchant configuration."},
	411: {openrails.DeclineMerchantConfig, "merchant_account_is_inactive", "Merchant account is inactive."},
	420: {openrails.DeclineCommunicationError, "communication_error", "Communication error."},
	421: {openrails.DeclineCommunicationError, "communication_error_with_issuer", "Communication error with issuer."},
	430: {openrails.DeclineDuplicateTransaction, "duplicate_transaction_at_processor", "Duplicate transaction at processor."},
	440: {openrails.DeclineInvalidRequest, "processor_format_error", "Processor format error."},
	441: {openrails.DeclineInvalidRequest, "invalid_transaction_information", "Invalid transaction information."},
	460: {openrails.DeclineInvalidRequest, "processor_feature_not_available", "Processor feature not available."},
	461: {openrails.DeclineCardNotSupported, "unsupported_card_type", "Unsupported card type."},
}

var nmiByLocalization = func() map[string]int {
	m := make(map[string]int, len(nmiCodes))
	for code, c := range nmiCodes {
		m[c.localization] = code
	}
	return m
}()

func nmiReasons() map[string]openrails.DeclineReason {
	m := make(map[string]openrails.DeclineReason, len(nmiCodes))
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

var stripeCodes = map[string]openrails.DeclineReason{
	"generic_decline":                       openrails.DeclineGeneric,
	"card_declined":                         openrails.DeclineGeneric,
	"do_not_honor":                          openrails.DeclineDoNotHonor,
	"no_action_taken":                       openrails.DeclineDoNotHonor,
	"call_issuer":                           openrails.DeclineCallIssuer,
	"insufficient_funds":                    openrails.DeclineInsufficientFunds,
	"withdrawal_count_limit_exceeded":       openrails.DeclineOverLimit,
	"card_velocity_exceeded":                openrails.DeclineOverLimit,
	"try_again_later":                       openrails.DeclineTryAgainLater,
	"reenter_transaction":                   openrails.DeclineTryAgainLater,
	"approve_with_id":                       openrails.DeclineTryAgainLater,
	"security_violation":                    openrails.DeclineSecurityViolation,
	"restricted_card":                       openrails.DeclineRestrictedCard,
	"incorrect_number":                      openrails.DeclineIncorrectNumber,
	"invalid_number":                        openrails.DeclineIncorrectNumber,
	"invalid_account":                       openrails.DeclineInvalidAccount,
	"no_account":                            openrails.DeclineInvalidAccount,
	"expired_card":                          openrails.DeclineExpiredCard,
	"invalid_expiry_month":                  openrails.DeclineInvalidExpiry,
	"invalid_expiry_year":                   openrails.DeclineInvalidExpiry,
	"incorrect_cvc":                         openrails.DeclineIncorrectCVC,
	"invalid_cvc":                           openrails.DeclineIncorrectCVC,
	"incorrect_zip":                         openrails.DeclineIncorrectZip,
	"incorrect_address":                     openrails.DeclineIncorrectAddress,
	"incorrect_pin":                         openrails.DeclineInvalidPIN,
	"invalid_pin":                           openrails.DeclineInvalidPIN,
	"pin_try_exceeded":                      openrails.DeclineInvalidPIN,
	"offline_pin_required":                  openrails.DeclineInvalidPIN,
	"online_or_offline_pin_required":        openrails.DeclineInvalidPIN,
	"transaction_not_allowed":               openrails.DeclineTransactionNotAllowed,
	"not_permitted":                         openrails.DeclineTransactionNotAllowed,
	"card_not_supported":                    openrails.DeclineCardNotSupported,
	"currency_not_supported":                openrails.DeclineCurrencyNotSupported,
	"authentication_required":               openrails.DeclineAuthenticationRequired,
	"payment_intent_authentication_failure": openrails.DeclineAuthenticationRequired,
	"pickup_card":                           openrails.DeclinePickupCard,
	"lost_card":                             openrails.DeclineLostCard,
	"stolen_card":                           openrails.DeclineStolenCard,
	"fraudulent":                            openrails.DeclineFraudulent,
	"stop_payment_order":                    openrails.DeclineStopRecurring,
	"revocation_of_authorization":           openrails.DeclineStopRecurring,
	"revocation_of_all_authorizations":      openrails.DeclineStopRecurring,
	"merchant_blacklist":                    openrails.DeclineBlockedByPSP,
	"processing_error":                      openrails.DeclineProcessingError,
	"issuer_not_available":                  openrails.DeclineIssuerUnavailable,
	"duplicate_transaction":                 openrails.DeclineDuplicateTransaction,
}

// ccbillCodes is CCBill's own BE-nnn vocabulary
// (ccbill.com/kb/list-of-credit-card-declined-codes). BE-900..999 are system
// errors.
func ccbillCodes() map[string]openrails.DeclineReason {
	m := map[string]openrails.DeclineReason{
		"be101": openrails.DeclineMerchantConfig,
		"be102": openrails.DeclinePickupCard,
		"be103": openrails.DeclineDoNotHonor,
		"be105": openrails.DeclineInvalidRequest,
		"be107": openrails.DeclineIncorrectNumber,
		"be112": openrails.DeclineInvalidAccount,
		"be113": openrails.DeclineInsufficientFunds,
		"be114": openrails.DeclineExpiredCard,
		"be116": openrails.DeclineTransactionNotAllowed,
		"be119": openrails.DeclineOverLimit,
		"be130": openrails.DeclineInvalidRequest,
		"be132": openrails.DeclineBlockedByPSP, // card blocked by CCBill
		"be146": openrails.DeclineBlockedByPSP, // blocked country
	}
	for n := 900; n <= 999; n++ {
		m["be"+strconv.Itoa(n)] = openrails.DeclineProcessingError
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
var solanaCodes = map[string]openrails.DeclineReason{
	"insufficient_funds":                   openrails.DeclineInsufficientFunds,
	"do_not_honor":                         openrails.DeclineDoNotHonor,
	"generic_decline":                      openrails.DeclineGeneric,
	"declined_update_cardholder_data":      openrails.DeclineUpdateCardholderData,
	"declined_stop_all_recurring_payments": openrails.DeclineStopRecurring,
	"declined_stop_this_recurring_program": openrails.DeclineStopRecurring,
	"duplicate_transaction":                openrails.DeclineDuplicateTransaction,
	"merchant_configuration_error":         openrails.DeclineMerchantConfig,
	"communication_error":                  openrails.DeclineCommunicationError,
	"processing_error":                     openrails.DeclineProcessingError,
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
