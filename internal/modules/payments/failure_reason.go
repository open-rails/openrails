package payments

import (
	"strconv"
	"strings"
)

// Normalized failure_reason vocabulary (#733). The raw rail code is always
// recorded verbatim in failure_code; this file maps it DETERMINISTICALLY onto
// a fixed category set. Unmapped codes read "unknown" — never fabricated.
// Attempt kinds stamped on payments at write time (#733).
const (
	AttemptInitial = "initial"
	AttemptRenewal = "renewal"
)

const (
	FailureInsufficientFunds    = "insufficient_funds"
	FailureCardDeclined         = "card_declined"
	FailureExpiredCard          = "expired_card"
	FailureCVVAVS               = "cvv_avs"
	FailureCardUnsupported      = "card_unsupported"
	FailureStopRecurring        = "stop_recurring"
	FailureDuplicateTransaction = "duplicate_transaction"
	FailureFraudSuspected       = "fraud_suspected"
	FailureProcessorError       = "processor_error"
	FailureConfigError          = "config_error"
	FailureGenericDecline       = "generic_decline"
	FailureUnknown              = "unknown"
)

// nmiFailureReasons maps NMI numeric response codes (recorded verbatim as the
// failure_code) to categories. Source: NMI response-code documentation.
var nmiFailureReasons = map[int]string{
	200: FailureCardDeclined,      // declined by processor
	201: FailureCardDeclined,      // do not honor
	202: FailureInsufficientFunds, // insufficient funds
	203: FailureInsufficientFunds, // over limit
	204: FailureCardDeclined,      // transaction not allowed
	220: FailureCVVAVS,            // incorrect payment information
	221: FailureCardDeclined,      // no such card issuer
	222: FailureCardDeclined,      // no card number on file with issuer
	223: FailureExpiredCard,       // expired card
	224: FailureExpiredCard,       // invalid expiration date
	225: FailureCVVAVS,            // invalid card security code
	226: FailureCVVAVS,            // invalid PIN
	240: FailureCardDeclined,      // call issuer for further information
	250: FailureFraudSuspected,    // pickup card
	251: FailureFraudSuspected,    // lost card
	252: FailureFraudSuspected,    // stolen card
	253: FailureFraudSuspected,    // fraudulent card
	260: FailureCardDeclined,      // declined with further instructions
	261: FailureStopRecurring,     // declined - stop all recurring payments
	262: FailureStopRecurring,     // declined - stop this recurring program
	263: FailureCardDeclined,      // declined - update cardholder data
	264: FailureCardDeclined,      // declined - retry in a few days
	300: FailureProcessorError,    // transaction rejected by gateway
	400: FailureProcessorError,    // transaction error returned by processor
	410: FailureConfigError,       // invalid merchant configuration
	411: FailureConfigError,       // merchant account inactive
	420: FailureProcessorError,    // communication error
	421: FailureProcessorError,    // communication error with issuer
	430: FailureDuplicateTransaction,
	440: FailureProcessorError, // processor format error
	441: FailureProcessorError, // invalid transaction information
	460: FailureProcessorError, // processor feature not available
	461: FailureCardUnsupported,
}

// nmiLocalizationFailureReasons maps NMI's string localization ids (the
// verbatim failure_code the seam rails record when the gateway supplies one —
// nmidirect.FailureCode prefers it over the numeric code) onto the same
// categories as the numeric table above.
var nmiLocalizationFailureReasons = map[string]string{
	"transaction_was_declined_by_processor": FailureCardDeclined,
	"do_not_honor":                          FailureCardDeclined,
	"insufficient_funds":                    FailureInsufficientFunds,
	"over_limit":                            FailureInsufficientFunds,
	"transaction_not_allowed":               FailureCardDeclined,
	"incorrect_payment_information":         FailureCVVAVS,
	"no_such_card_issuer":                   FailureCardDeclined,
	"no_card_number_on_file_with_issuer":    FailureCardDeclined,
	"expired_card":                          FailureExpiredCard,
	"invalid_expiration_date":               FailureExpiredCard,
	"invalid_card_security_code":            FailureCVVAVS,
	"invalid_pin":                           FailureCVVAVS,
	"call_issuer_for_further_information":   FailureCardDeclined,
	"pick_up_card":                          FailureFraudSuspected,
	"lost_card":                             FailureFraudSuspected,
	"stolen_card":                           FailureFraudSuspected,
	"fraudulent_card":                       FailureFraudSuspected,
	"declined_with_further_instructions_available_see_response_text": FailureCardDeclined,
	"declined_stop_all_recurring_payments":                           FailureStopRecurring,
	"declined_stop_this_recurring_program":                           FailureStopRecurring,
	"declined_update_cardholder_data_available":                      FailureCardDeclined,
	"declined_retry_in_a_few_days":                                   FailureCardDeclined,
	"transaction_was_rejected_by_gateway":                            FailureProcessorError,
	"transaction_error_returned_by_processor":                        FailureProcessorError,
	"invalid_merchant_configuration":                                 FailureConfigError,
	"merchant_account_is_inactive":                                   FailureConfigError,
	"communication_error":                                            FailureProcessorError,
	"communication_error_with_issuer":                                FailureProcessorError,
	"duplicate_transaction_at_rail":                                  FailureDuplicateTransaction,
	"rail_format_error":                                              FailureProcessorError,
	"invalid_transaction_information":                                FailureProcessorError,
	"rail_feature_not_available":                                     FailureProcessorError,
	"unsupported_card_type":                                          FailureCardUnsupported,
}

// stripeFailureReasons maps Stripe decline_code / failure_code strings.
var stripeFailureReasons = map[string]string{
	"insufficient_funds":               FailureInsufficientFunds,
	"withdrawal_count_limit_exceeded":  FailureInsufficientFunds,
	"card_declined":                    FailureCardDeclined,
	"generic_decline":                  FailureGenericDecline,
	"do_not_honor":                     FailureCardDeclined,
	"transaction_not_allowed":          FailureCardDeclined,
	"call_issuer":                      FailureCardDeclined,
	"card_velocity_exceeded":           FailureCardDeclined,
	"expired_card":                     FailureExpiredCard,
	"incorrect_cvc":                    FailureCVVAVS,
	"invalid_cvc":                      FailureCVVAVS,
	"incorrect_zip":                    FailureCVVAVS,
	"incorrect_number":                 FailureCardDeclined,
	"invalid_number":                   FailureCardDeclined,
	"lost_card":                        FailureFraudSuspected,
	"stolen_card":                      FailureFraudSuspected,
	"pickup_card":                      FailureFraudSuspected,
	"fraudulent":                       FailureFraudSuspected,
	"security_violation":               FailureFraudSuspected,
	"card_not_supported":               FailureCardUnsupported,
	"currency_not_supported":           FailureCardUnsupported,
	"stop_payment_order":               FailureStopRecurring,
	"revocation_of_all_authorizations": FailureStopRecurring,
	"revocation_of_authorization":      FailureStopRecurring,
	"duplicate_transaction":            FailureDuplicateTransaction,
	"processing_error":                 FailureProcessorError,
	"issuer_not_available":             FailureProcessorError,
	"try_again_later":                  FailureProcessorError,
}

// solanaFailureReasons maps the rail-agnostic declinecode vocabulary the
// Solana crank classifies on-chain failures into (internal/billing/declinecode).
var solanaFailureReasons = map[string]string{
	"insufficient_funds":                   FailureInsufficientFunds,
	"do_not_honor":                         FailureCardDeclined,
	"declined_stop_all_recurring_payments": FailureStopRecurring,
	"duplicate_transaction":                FailureDuplicateTransaction,
	"merchant_configuration_error":         FailureConfigError,
	"communication_error":                  FailureProcessorError,
	"processing_error":                     FailureProcessorError,
	"generic_decline":                      FailureGenericDecline,
	"declined_update_cardholder_data":      FailureCardDeclined,
	"declined_stop_this_recurring_program": FailureStopRecurring,
}

// ccbillFailureReasons: CCBill webhook failureCode values. CCBill's decline
// vocabulary is not publicly enumerated; only codes verified against live
// webhook traffic get mapped, the rest read "unknown" with the verbatim code
// preserved (documented as partial coverage in /schema caveats).
var ccbillFailureReasons = map[string]string{}

// DefaultTokenTypeForRail is the #796 token_type stamp for charge records
// created OFF the seam (provider-driven renewals, webhook/converge folds),
// where no charge.Result carries the rail's answer: NMI charges its own
// customer vault; vaulted_card proxies the detokenized FPAN (NT charging is
// config-gated off on NMI gateways). "" = not a stamped card rail.
func DefaultTokenTypeForRail(rail string) string {
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case "nmi", "mobius":
		return "provider_vault"
	case "vaulted_card":
		return "pan_via_vault"
	default:
		return ""
	}
}

// NormalizeFailureReason maps a rail's raw decline code to the normalized
// failure_reason vocabulary. Deterministic: same input, same output.
func NormalizeFailureReason(rail, rawCode string) string {
	if rawCode == "" {
		return FailureUnknown
	}
	switch rail {
	case "nmi", "mobius", "vaulted_card":
		// vaulted_card parses NMI classic responses through the same taxonomy
		// (#795: one decline vocabulary, two transports).
		if n, err := strconv.Atoi(rawCode); err == nil {
			if r, ok := nmiFailureReasons[n]; ok {
				return r
			}
			return FailureUnknown
		}
		if r, ok := nmiLocalizationFailureReasons[rawCode]; ok {
			return r
		}
		return FailureUnknown
	case "stripe":
		if r, ok := stripeFailureReasons[rawCode]; ok {
			return r
		}
		return FailureUnknown
	case "solana":
		if r, ok := solanaFailureReasons[rawCode]; ok {
			return r
		}
		return FailureUnknown
	case "ccbill":
		if r, ok := ccbillFailureReasons[rawCode]; ok {
			return r
		}
		return FailureUnknown
	default:
		return FailureUnknown
	}
}
