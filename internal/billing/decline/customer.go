package decline

import (
	"strings"

	"github.com/open-rails/openrails"
)

// Buyer-facing reasons. Raw codes stay in operator evidence; buyers only see
// these reasons and their copy.
const (
	CustomerIncorrectCVC           = "incorrect_cvc"
	CustomerIncorrectZip           = "incorrect_zip"
	CustomerIncorrectAddress       = "incorrect_address"
	CustomerIncorrectNumber        = "incorrect_number"
	CustomerExpiredCard            = "expired_card"
	CustomerInvalidExpiry          = "invalid_expiry"
	CustomerInsufficientFunds      = "insufficient_funds"
	CustomerOverLimit              = "over_limit"
	CustomerCardNotSupported       = "card_not_supported"
	CustomerCurrencyNotSupported   = "currency_not_supported"
	CustomerProcessingError        = "processing_error"
	CustomerTryAgainLater          = "try_again_later"
	CustomerAuthenticationRequired = "authentication_required"
	CustomerDoNotHonor             = "do_not_honor"
	CustomerGeneric                = "generic_decline"
)

var customerCopy = map[string]struct{ message, field string }{
	CustomerIncorrectCVC:           {"The card's security code is incorrect.", "cvc"},
	CustomerIncorrectZip:           {"The postal code doesn't match the card.", "postal_code"},
	CustomerIncorrectAddress:       {"The billing address doesn't match the card.", ""},
	CustomerIncorrectNumber:        {"The card number is incorrect.", "number"},
	CustomerExpiredCard:            {"The card has expired.", "expiry"},
	CustomerInvalidExpiry:          {"The card's expiration date is incorrect.", "expiry"},
	CustomerInsufficientFunds:      {"The card has insufficient funds.", ""},
	CustomerOverLimit:              {"The card is over its limit.", ""},
	CustomerCardNotSupported:       {"This card doesn't support this type of purchase.", ""},
	CustomerCurrencyNotSupported:   {"This card doesn't support this currency.", ""},
	CustomerProcessingError:        {"The payment couldn't be processed. Try again.", ""},
	CustomerTryAgainLater:          {"The card issuer is unavailable. Try again later.", ""},
	CustomerAuthenticationRequired: {"The card issuer requires authentication. Try again and complete the verification.", ""},
	CustomerDoNotHonor:             {"Your card was declined. Contact your bank or try a different card.", ""},
	CustomerGeneric:                {"Your card was declined. Contact your bank or try a different card.", ""},
}

// PaymentFailure is what the buyer is told about this refusal.
func (r Result) PaymentFailure() openrails.PaymentFailure {
	return Failure(reasons[r.Reason].customer)
}

// Failure renders a buyer-facing reason; an unknown one reads generic_decline.
func Failure(customer string) openrails.PaymentFailure {
	customer = strings.TrimSpace(customer)
	c, ok := customerCopy[customer]
	if !ok {
		customer, c = CustomerGeneric, customerCopy[CustomerGeneric]
	}
	return openrails.PaymentFailure{Reason: customer, Message: c.message, Field: c.field}
}
