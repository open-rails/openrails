package decline

import (
	"strconv"
	"strings"
)

type railTable struct {
	codes     map[string]Reason
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
	reason       Reason
	localization string
	message      string
}

// nmiCodes is NMI's published response_code table
// (docs.nmi.com/reference/response-codes).
var nmiCodes = map[int]nmiCode{
	100: {"", "transaction_was_approved", "Transaction was approved."},
	200: {GenericDecline, "transaction_was_declined_by_processor", "Transaction was declined by processor."},
	201: {DoNotHonor, "do_not_honor", "Do not honor."},
	202: {InsufficientFunds, "insufficient_funds", "Insufficient funds."},
	203: {OverLimit, "over_limit", "Over limit."},
	204: {TransactionNotAllowed, "transaction_not_allowed", "Transaction not allowed."},
	220: {IncorrectNumber, "incorrect_payment_information", "Incorrect payment information."},
	221: {NoSuchIssuer, "no_such_card_issuer", "No such card issuer."},
	222: {InvalidAccount, "no_card_number_on_file_with_issuer", "No card number on file with issuer."},
	223: {ExpiredCard, "expired_card", "Expired card."},
	224: {InvalidExpiry, "invalid_expiration_date", "Invalid expiration date."},
	225: {IncorrectCVC, "invalid_card_security_code", "Invalid card security code."},
	226: {InvalidPIN, "invalid_pin", "Invalid PIN."},
	240: {CallIssuer, "call_issuer_for_further_information", "Call issuer for further information."},
	250: {PickupCard, "pick_up_card", "Pick up card."},
	251: {LostCard, "lost_card", "Lost card."},
	252: {StolenCard, "stolen_card", "Stolen card."},
	253: {Fraudulent, "fraudulent_card", "Fraudulent card."},
	260: {GenericDecline, "declined_with_further_instructions_available_see_response_text", "Declined with further instructions available. (See response text)"},
	261: {StopRecurring, "declined_stop_all_recurring_payments", "Declined-Stop all recurring payments."},
	262: {StopRecurring, "declined_stop_this_recurring_program", "Declined-Stop this recurring program."},
	263: {UpdateCardholderData, "declined_update_cardholder_data_available", "Declined-Update cardholder data available."},
	264: {RetryLater, "declined_retry_in_a_few_days", "Declined-Retry in a few days."},
	300: {GatewayRejected, "transaction_was_rejected_by_gateway", "Transaction was rejected by gateway."},
	400: {ProcessingError, "transaction_error_returned_by_processor", "Transaction error returned by processor."},
	410: {MerchantConfig, "invalid_merchant_configuration", "Invalid merchant configuration."},
	411: {MerchantConfig, "merchant_account_is_inactive", "Merchant account is inactive."},
	420: {CommunicationError, "communication_error", "Communication error."},
	421: {CommunicationError, "communication_error_with_issuer", "Communication error with issuer."},
	430: {DuplicateTransaction, "duplicate_transaction_at_processor", "Duplicate transaction at processor."},
	440: {InvalidRequest, "processor_format_error", "Processor format error."},
	441: {InvalidRequest, "invalid_transaction_information", "Invalid transaction information."},
	460: {InvalidRequest, "processor_feature_not_available", "Processor feature not available."},
	461: {CardNotSupported, "unsupported_card_type", "Unsupported card type."},
}

var nmiByLocalization = func() map[string]int {
	m := make(map[string]int, len(nmiCodes))
	for code, c := range nmiCodes {
		m[c.localization] = code
	}
	return m
}()

func nmiReasons() map[string]Reason {
	m := make(map[string]Reason, len(nmiCodes))
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

var stripeCodes = map[string]Reason{
	"generic_decline":                       GenericDecline,
	"card_declined":                         GenericDecline,
	"do_not_honor":                          DoNotHonor,
	"no_action_taken":                       DoNotHonor,
	"call_issuer":                           CallIssuer,
	"insufficient_funds":                    InsufficientFunds,
	"withdrawal_count_limit_exceeded":       OverLimit,
	"card_velocity_exceeded":                OverLimit,
	"try_again_later":                       TryAgainLater,
	"reenter_transaction":                   TryAgainLater,
	"approve_with_id":                       TryAgainLater,
	"security_violation":                    SecurityViolation,
	"restricted_card":                       RestrictedCard,
	"incorrect_number":                      IncorrectNumber,
	"invalid_number":                        IncorrectNumber,
	"invalid_account":                       InvalidAccount,
	"no_account":                            InvalidAccount,
	"expired_card":                          ExpiredCard,
	"invalid_expiry_month":                  InvalidExpiry,
	"invalid_expiry_year":                   InvalidExpiry,
	"incorrect_cvc":                         IncorrectCVC,
	"invalid_cvc":                           IncorrectCVC,
	"incorrect_zip":                         IncorrectZip,
	"incorrect_address":                     IncorrectAddress,
	"incorrect_pin":                         InvalidPIN,
	"invalid_pin":                           InvalidPIN,
	"pin_try_exceeded":                      InvalidPIN,
	"offline_pin_required":                  InvalidPIN,
	"online_or_offline_pin_required":        InvalidPIN,
	"transaction_not_allowed":               TransactionNotAllowed,
	"not_permitted":                         TransactionNotAllowed,
	"card_not_supported":                    CardNotSupported,
	"currency_not_supported":                CurrencyNotSupported,
	"authentication_required":               AuthenticationRequired,
	"payment_intent_authentication_failure": AuthenticationRequired,
	"pickup_card":                           PickupCard,
	"lost_card":                             LostCard,
	"stolen_card":                           StolenCard,
	"fraudulent":                            Fraudulent,
	"stop_payment_order":                    StopRecurring,
	"revocation_of_authorization":           StopRecurring,
	"revocation_of_all_authorizations":      StopRecurring,
	"merchant_blacklist":                    BlockedByPSP,
	"processing_error":                      ProcessingError,
	"issuer_not_available":                  IssuerUnavailable,
	"duplicate_transaction":                 DuplicateTransaction,
}

// ccbillCodes is CCBill's own BE-nnn vocabulary
// (ccbill.com/kb/list-of-credit-card-declined-codes). BE-900..999 are system
// errors.
func ccbillCodes() map[string]Reason {
	m := map[string]Reason{
		"be101": MerchantConfig,
		"be102": PickupCard,
		"be103": DoNotHonor,
		"be105": InvalidRequest,
		"be107": IncorrectNumber,
		"be112": InvalidAccount,
		"be113": InsufficientFunds,
		"be114": ExpiredCard,
		"be116": TransactionNotAllowed,
		"be119": OverLimit,
		"be130": InvalidRequest,
		"be132": BlockedByPSP, // card blocked by CCBill
		"be146": BlockedByPSP, // blocked country
	}
	for n := 900; n <= 999; n++ {
		m["be"+strconv.Itoa(n)] = ProcessingError
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
var solanaCodes = map[string]Reason{
	"insufficient_funds":                   InsufficientFunds,
	"do_not_honor":                         DoNotHonor,
	"generic_decline":                      GenericDecline,
	"declined_update_cardholder_data":      UpdateCardholderData,
	"declined_stop_all_recurring_payments": StopRecurring,
	"declined_stop_this_recurring_program": StopRecurring,
	"duplicate_transaction":                DuplicateTransaction,
	"merchant_configuration_error":         MerchantConfig,
	"communication_error":                  CommunicationError,
	"processing_error":                     ProcessingError,
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
