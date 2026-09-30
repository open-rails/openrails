package handlers

import (
	"net/http"
	"strings"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/billing/decline"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/api"
)

// writePaymentMethodError renders a provider refusal as the shared payment
// refusal contract (openrails.CodeCardDeclined / CodePaymentProviderRejected).
// Classification uses the provider's code and AVS/CVV results; processor text
// stays in server-side logs and the caller receives safe actionable copy.
func writePaymentMethodError(r *httprequest.Request, pmErr *paymentmethods.PaymentMethodError) {
	r.APIError(railPaymentRefusalError(pmErr.Rail, strings.TrimSpace(pmErr.LocalizationID), pmErr.Reason))
}

// writePaymentMethodStale renders openrails.CodePaymentMethodStale: the saved
// instrument the request named can no longer be charged; collect it again.
func writePaymentMethodStale(r *httprequest.Request) {
	r.APIError(api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, openrails.CodePaymentMethodStale,
		"This saved payment method can no longer be used. Add the card again."))
}

// writePaymentMethodRequired renders openrails.CodePaymentMethodRequired: a
// charge must name its payment method; none is implied.
func writePaymentMethodRequired(r *httprequest.Request) {
	param := "payment_method_id"
	e := api.NewAPIError(http.StatusBadRequest, api.ErrorTypeInvalidRequest, openrails.CodePaymentMethodRequired,
		"Choose a payment method: name a saved payment_method_id or send a new card payment_token.")
	e.Param = &param
	r.APIError(e)
}

// paymentRefusalError maps an NMI failure code (localization id or numeric
// response code) onto the refusal envelope. Card categories are a 402
// card_declined the customer can answer with another card; gateway and
// merchant-configuration categories are a 502 payment_provider_rejected.
func paymentRefusalError(failureCode string) *api.APIError {
	return railPaymentRefusalError("nmi", failureCode, "")
}

// railPaymentRefusalError renders a refusal: reason, when set, was classified
// with the refusal's full evidence; otherwise failureCode is classified in
// rail's vocabulary.
func railPaymentRefusalError(rail, failureCode string, reason openrails.DeclineReason) *api.APIError {
	if rail == "" {
		rail = "nmi"
	}
	if reason == "" {
		reason = decline.Classify(rail, failureCode).Reason
	}
	failure := reason.Failure()
	metadata := map[string]any{"decline_reason": string(reason), "failure": failure}
	if failureCode != "" {
		metadata["failure_code"] = failureCode
	}
	switch {
	case decline.ProviderFault(reason):
		return api.NewAPIError(http.StatusBadGateway, api.ErrorTypeAPI, openrails.CodePaymentProviderRejected,
			"The payment processor could not complete this payment. Please try again later.").WithMetadata(metadata)
	default:
		return api.NewAPIError(http.StatusPaymentRequired, api.ErrorTypeCard, openrails.CodeCardDeclined,
			failure.Message).WithMetadata(metadata)
	}
}
