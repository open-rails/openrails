package billing

import (
	"errors"
	"slices"
)

// DeclineReason is why a payment was refused. OpenRails maps every rail's
// decline code (NMI response codes, Stripe decline codes, CCBill BE codes)
// onto one reason. It is the decline_reason metadata of a card_declined or
// payment_provider_rejected error; read it with DeclineReasonFrom.
type DeclineReason string

const (
	// The issuer declined a well-formed request that may pass later.
	DeclineGeneric           DeclineReason = "generic_decline"
	DeclineDoNotHonor        DeclineReason = "do_not_honor"
	DeclineInsufficientFunds DeclineReason = "insufficient_funds"
	DeclineOverLimit         DeclineReason = "over_limit"
	DeclineCallIssuer        DeclineReason = "call_issuer"
	DeclineRetryLater        DeclineReason = "retry_later"
	DeclineTryAgainLater     DeclineReason = "try_again_later"
	DeclineSecurityViolation DeclineReason = "security_violation"
	DeclineRestrictedCard    DeclineReason = "restricted_card"

	// The card details don't match an open, valid account.
	DeclineIncorrectNumber      DeclineReason = "incorrect_number"
	DeclineNoSuchIssuer         DeclineReason = "no_such_issuer"
	DeclineInvalidAccount       DeclineReason = "invalid_account"
	DeclineExpiredCard          DeclineReason = "expired_card"
	DeclineInvalidExpiry        DeclineReason = "invalid_expiry"
	DeclineIncorrectCVC         DeclineReason = "incorrect_cvc"
	DeclineIncorrectZip         DeclineReason = "incorrect_zip"
	DeclineIncorrectAddress     DeclineReason = "incorrect_address"
	DeclineInvalidPIN           DeclineReason = "invalid_pin"
	DeclineUpdateCardholderData DeclineReason = "update_cardholder_data"

	// The issuer will not approve this card for this purchase.
	DeclineTransactionNotAllowed  DeclineReason = "transaction_not_allowed"
	DeclineCardNotSupported       DeclineReason = "card_not_supported"
	DeclineCurrencyNotSupported   DeclineReason = "currency_not_supported"
	DeclineAuthenticationRequired DeclineReason = "authentication_required"
	DeclinePickupCard             DeclineReason = "pickup_card"
	DeclineLostCard               DeclineReason = "lost_card"
	DeclineStolenCard             DeclineReason = "stolen_card"
	DeclineFraudulent             DeclineReason = "fraudulent"
	DeclineStopRecurring          DeclineReason = "stop_recurring"

	// The PSP's own rule refused it; the issuer never saw it.
	DeclineGatewayRejected DeclineReason = "gateway_rejected"
	DeclineBlockedByPSP    DeclineReason = "blocked_by_psp"

	// The gateway, the processor or the merchant's configuration failed.
	DeclineProcessingError      DeclineReason = "processing_error"
	DeclineCommunicationError   DeclineReason = "communication_error"
	DeclineIssuerUnavailable    DeclineReason = "issuer_unavailable"
	DeclineInvalidRequest       DeclineReason = "invalid_request"
	DeclineMerchantConfig       DeclineReason = "merchant_config"
	DeclineDuplicateTransaction DeclineReason = "duplicate_transaction"

	// DeclineUnknown is a code no table maps.
	DeclineUnknown DeclineReason = "unknown"
)

// declineCustomer is what each reason tells the buyer (PaymentFailure.Reason).
var declineCustomer = map[DeclineReason]string{
	DeclineGeneric: "generic_decline", DeclineDoNotHonor: "do_not_honor", DeclineInsufficientFunds: "insufficient_funds",
	DeclineOverLimit: "over_limit", DeclineCallIssuer: "do_not_honor", DeclineRetryLater: "try_again_later",
	DeclineTryAgainLater: "try_again_later", DeclineSecurityViolation: "generic_decline", DeclineRestrictedCard: "generic_decline",

	DeclineIncorrectNumber: "incorrect_number", DeclineNoSuchIssuer: "incorrect_number", DeclineInvalidAccount: "incorrect_number",
	DeclineExpiredCard: "expired_card", DeclineInvalidExpiry: "invalid_expiry", DeclineIncorrectCVC: "incorrect_cvc",
	DeclineIncorrectZip: "incorrect_zip", DeclineIncorrectAddress: "incorrect_address", DeclineInvalidPIN: "generic_decline",
	DeclineUpdateCardholderData: "do_not_honor",

	DeclineTransactionNotAllowed: "card_not_supported", DeclineCardNotSupported: "card_not_supported",
	DeclineCurrencyNotSupported: "currency_not_supported", DeclineAuthenticationRequired: "authentication_required",
	DeclinePickupCard: "generic_decline", DeclineLostCard: "generic_decline", DeclineStolenCard: "generic_decline",
	DeclineFraudulent: "generic_decline", DeclineStopRecurring: "do_not_honor",

	DeclineGatewayRejected: "processing_error", DeclineBlockedByPSP: "generic_decline",

	DeclineProcessingError: "processing_error", DeclineCommunicationError: "try_again_later",
	DeclineIssuerUnavailable: "try_again_later", DeclineInvalidRequest: "processing_error",
	DeclineMerchantConfig: "processing_error", DeclineDuplicateTransaction: "processing_error",

	DeclineUnknown: "generic_decline",
}

// DeclineReasons lists every reason, sorted.
func DeclineReasons() []DeclineReason {
	out := make([]DeclineReason, 0, len(declineCustomer))
	for r := range declineCustomer {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// FraudSignal reports a reason the buyer is never told: the card was reported
// lost or stolen, or a risk rule refused it.
func (r DeclineReason) FraudSignal() bool {
	switch r {
	case DeclinePickupCard, DeclineLostCard, DeclineStolenCard, DeclineFraudulent, DeclineSecurityViolation, DeclineRestrictedCard, DeclineBlockedByPSP:
		return true
	}
	return false
}

var failureCopy = map[string]struct{ message, field string }{
	"incorrect_cvc":           {"The card's security code is incorrect.", "cvc"},
	"incorrect_zip":           {"The postal code doesn't match the card.", "postal_code"},
	"incorrect_address":       {"The billing address doesn't match the card.", ""},
	"incorrect_number":        {"The card number is incorrect.", "number"},
	"expired_card":            {"The card has expired.", "expiry"},
	"invalid_expiry":          {"The card's expiration date is incorrect.", "expiry"},
	"insufficient_funds":      {"The card has insufficient funds.", ""},
	"over_limit":              {"The card is over its limit.", ""},
	"card_not_supported":      {"This card doesn't support this type of purchase.", ""},
	"currency_not_supported":  {"This card doesn't support this currency.", ""},
	"processing_error":        {"The payment couldn't be processed. Try again.", ""},
	"try_again_later":         {"The card issuer is unavailable. Try again later.", ""},
	"authentication_required": {"The card issuer requires authentication. Try again and complete the verification.", ""},
	"do_not_honor":            {"Your card was declined. Contact your bank or try a different card.", ""},
	"generic_decline":         {"Your card was declined. Contact your bank or try a different card.", ""},
}

// Failure is what to tell the buyer about a refusal with this reason: a
// provider-neutral reason, OpenRails' message, and the card field to fix.
// Fraud signals and unknown reasons read generic_decline.
func (r DeclineReason) Failure() PaymentFailure {
	reason := declineCustomer[r]
	c, ok := failureCopy[reason]
	if !ok {
		reason, c = "generic_decline", failureCopy["generic_decline"]
	}
	return PaymentFailure{Reason: reason, Message: c.message, Field: c.field}
}

// DeclineReasonFrom returns the decline reason of a card_declined or
// payment_provider_rejected refusal.
func DeclineReasonFrom(err error) (DeclineReason, bool) {
	var status *StatusError
	if !errors.As(err, &status) || (status.Code != CodeCardDeclined && status.Code != CodePaymentProviderRejected) {
		return "", false
	}
	reason, _ := status.Metadata["decline_reason"].(string)
	return DeclineReason(reason), reason != ""
}
