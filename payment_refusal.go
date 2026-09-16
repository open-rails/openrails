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
)

var (
	ErrCardDeclined            error = newCodedError(CodeCardDeclined, ErrPaymentRefused)
	ErrPaymentMethodStale      error = newCodedError(CodePaymentMethodStale, ErrPaymentRefused)
	ErrPaymentProviderRejected error = newCodedError(CodePaymentProviderRejected, ErrInternal)
)
