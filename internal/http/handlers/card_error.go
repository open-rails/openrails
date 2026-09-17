package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/api"
)

// writePaymentMethodError renders a provider refusal as the shared payment
// refusal contract (openrails.CodeCardDeclined / CodePaymentProviderRejected).
// Classification uses only the provider's verbatim failure code; processor
// text stays in server-side logs and the caller receives safe actionable copy.
func writePaymentMethodError(r *httprequest.Request, pmErr *paymentmethods.PaymentMethodError) {
	r.APIError(paymentRefusalError(strings.TrimSpace(pmErr.LocalizationID)))
}

// writePaymentMethodStale renders openrails.CodePaymentMethodStale: the saved
// instrument the request named can no longer be charged; collect it again.
func writePaymentMethodStale(r *httprequest.Request) {
	r.APIError(api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, openrails.CodePaymentMethodStale,
		"This saved payment method can no longer be used. Add the card again."))
}

// paymentRefusalError maps an NMI failure code (localization id or numeric
// response code) onto the refusal envelope. Card categories are a 402
// card_declined the customer can answer with another card; gateway and
// merchant-configuration categories are a 502 payment_provider_rejected.
func paymentRefusalError(failureCode string) *api.APIError {
	reason := payments.NormalizeFailureReason("nmi", failureCode)
	metadata := map[string]any{"decline_reason": reason}
	if failureCode != "" {
		metadata["failure_code"] = failureCode
	}
	switch reason {
	case payments.FailureProcessorError, payments.FailureConfigError:
		return api.NewAPIError(http.StatusBadGateway, api.ErrorTypeAPI, openrails.CodePaymentProviderRejected,
			"The payment processor could not complete this payment. Please try again later.").WithMetadata(metadata)
	default:
		return api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, openrails.CodeCardDeclined,
			cardDeclinedMessage(reason)).WithMetadata(metadata)
	}
}

func cardDeclinedMessage(reason string) string {
	switch reason {
	case payments.FailureInsufficientFunds:
		return "Your card has insufficient funds."
	case payments.FailureExpiredCard:
		return "Your card is expired. Use a different card."
	case payments.FailureCVVAVS:
		return "Check your card security code and billing details, then try again."
	case payments.FailureCardUnsupported:
		return "This card is not supported. Try a different card."
	case payments.FailureStopRecurring:
		return "Your bank stopped this recurring payment. Contact your bank or try a different card."
	case payments.FailureDuplicateTransaction:
		return "This payment may be a duplicate. Wait a moment before trying again."
	case payments.FailureFraudSuspected:
		return "Your bank declined this payment. Contact your bank or try a different card."
	case payments.FailureCardDeclined, payments.FailureGenericDecline:
		return "Your card was declined. Contact your bank or try a different card."
	default:
		return "We could not complete this payment. Please try again or use a different card."
	}
}
