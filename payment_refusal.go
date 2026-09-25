package openrails

import "errors"

// ErrPaymentRefused classifies HTTP 402: the payment was not made and no money
// moved. Checkout refusals carry one of the codes below; the customer may try
// again with another instrument in a new checkout session.
var ErrPaymentRefused = errors.New("openrails: payment refused")

// Payment refusal codes. They are stable; human messages are not.
const (
	// CodeCardDeclined is a provider decline of the presented card (type
	// card_error, 402). Metadata: decline_reason is the normalized category
	// (insufficient_funds, expired_card, cvv_avs, card_declined, ...);
	// failure_code is the provider's verbatim code.
	CodeCardDeclined = "card_declined"
	// CodePaymentMethodStale means the saved payment method the request named
	// can no longer be charged for this customer and processor (deleted, not
	// theirs, vaulted elsewhere, or its collection token expired): collect the
	// card again (type card_error, 402).
	CodePaymentMethodStale = "payment_method_stale"
	// CodePaymentProviderRejected means the provider refused to process the
	// charge for a reason unrelated to the card — gateway or merchant account
	// configuration (type api_error, 502). Nothing was charged; another card
	// will not help.
	CodePaymentProviderRejected = "payment_provider_rejected"
	// CodePaymentMethodRequired refuses a charge that names neither a saved
	// payment method nor a new card token (type invalid_request_error, 400).
	// OpenRails never charges an implied card such as the default one.
	CodePaymentMethodRequired = "payment_method_required"
)

var (
	ErrCardDeclined            error = newCodedError(CodeCardDeclined, ErrPaymentRefused)
	ErrPaymentMethodStale      error = newCodedError(CodePaymentMethodStale, ErrPaymentRefused)
	ErrPaymentProviderRejected error = newCodedError(CodePaymentProviderRejected, ErrInternal)
	ErrPaymentMethodRequired   error = newCodedError(CodePaymentMethodRequired, ErrInvalid)
)

// PaymentFailureFrom returns the customer-facing decline carried by a
// card_declined refusal, for hosts that relay it to the buyer.
func PaymentFailureFrom(err error) (*PaymentFailure, bool) {
	var status *StatusError
	if !errors.As(err, &status) || status.Code != CodeCardDeclined {
		return nil, false
	}
	raw, ok := status.Metadata["failure"].(map[string]any)
	if !ok {
		return nil, false
	}
	failure := &PaymentFailure{}
	failure.Reason, _ = raw["reason"].(string)
	failure.Message, _ = raw["message"].(string)
	failure.Field, _ = raw["field"].(string)
	return failure, failure.Reason != ""
}
