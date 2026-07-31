package handlers

import (
	"net/http"
	"strings"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/api"
)

// writeVaultError translates NMI's raw decline vocabulary into OpenRails'
// stable public categories. Processor text and localization IDs stay in
// server-side logs; the browser receives only safe actionable copy.
func writeVaultError(r *httprequest.Request, vaultErr *paymentmethods.VaultError) {
	reason := payments.NormalizeFailureReason("nmi", strings.TrimSpace(vaultErr.LocalizationID))
	code, message := publicCardFailure(reason)
	r.APIError(api.NewAPIError(http.StatusBadRequest, api.ErrorTypeCard, code, message).
		WithMetadata(map[string]any{"decline_reason": reason}))
}

func publicCardFailure(reason string) (code, message string) {
	switch reason {
	case payments.FailureInsufficientFunds:
		return reason, "Your card has insufficient funds."
	case payments.FailureExpiredCard:
		return reason, "Your card is expired. Use a different card."
	case payments.FailureCVVAVS:
		return reason, "Check your card security code and billing details, then try again."
	case payments.FailureCardUnsupported:
		return reason, "This card is not supported. Try a different card."
	case payments.FailureStopRecurring:
		return reason, "Your bank stopped this recurring payment. Contact your bank or try a different card."
	case payments.FailureDuplicateTransaction:
		return reason, "This payment may be a duplicate. Wait a moment before trying again."
	case payments.FailureFraudSuspected:
		return reason, "Your bank declined this payment. Contact your bank or try a different card."
	case payments.FailureProcessorError, payments.FailureConfigError:
		return payments.FailureProcessorError, "The payment processor could not complete this payment. Please try again."
	case payments.FailureCardDeclined, payments.FailureGenericDecline:
		return reason, "Your card was declined. Contact your bank or try a different card."
	default:
		return api.CodePaymentFailed, "We could not complete this payment. Please try again or use a different card."
	}
}
