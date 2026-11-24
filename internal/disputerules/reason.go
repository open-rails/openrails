package disputerules

import (
	"strings"

	"github.com/open-rails/openrails/billing"
)

// Classify is the reason family of a dispute: the network's code when it
// names one, else the provider's reason. It follows Stripe's mapping
// (docs.stripe.com/disputes/categories). Mastercard's 4853 covers several
// families, so the provider's reason decides it when there is one.
func Classify(network billing.CardNetwork, code, providerReason string) billing.DisputeReason {
	code = strings.ToUpper(strings.TrimSpace(code))
	if family, ok := byCode(network, code); ok {
		return family
	}
	if family, ok := byProviderReason(providerReason); ok {
		return family
	}
	if code == "4853" {
		return billing.DisputeNotAsDescribed
	}
	return billing.DisputeUnknownReason
}

func byCode(network billing.CardNetwork, code string) (billing.DisputeReason, bool) {
	if code == "" {
		return "", false
	}
	switch {
	case network == billing.NetworkVisa || (network != billing.NetworkMastercard && visaShaped(code)):
		return visaCode(code)
	case network == billing.NetworkMastercard || mastercardShaped(code):
		return mastercardCode(code)
	case network == billing.NetworkAmex:
		return amexCode(code)
	case network == billing.NetworkDiscover:
		return discoverCode(code)
	}
	return "", false
}

func visaShaped(code string) bool {
	return len(code) >= 4 && code[2] == '.' && (strings.HasPrefix(code, "10") || strings.HasPrefix(code, "11") ||
		strings.HasPrefix(code, "12") || strings.HasPrefix(code, "13"))
}

func mastercardShaped(code string) bool { return len(code) == 4 && strings.HasPrefix(code, "48") }

func visaCode(code string) (billing.DisputeReason, bool) {
	switch {
	case strings.HasPrefix(code, "10."):
		return billing.DisputeFraud, true
	case strings.HasPrefix(code, "11."):
		return billing.DisputeAuthorization, true
	case code == "12.6" || strings.HasPrefix(code, "12.6."):
		return billing.DisputeDuplicate, true
	case strings.HasPrefix(code, "12."):
		return billing.DisputeProcessingError, true
	case code == "13.1":
		return billing.DisputeNotReceived, true
	case code == "13.2":
		return billing.DisputeCanceledRecurring, true
	case code == "13.3", code == "13.4", code == "13.5":
		return billing.DisputeNotAsDescribed, true
	case code == "13.6", code == "13.7":
		return billing.DisputeCreditNotProcessed, true
	case strings.HasPrefix(code, "13."):
		return billing.DisputeOtherReason, true
	}
	return "", false
}

func mastercardCode(code string) (billing.DisputeReason, bool) {
	switch code {
	case "4837", "4840", "4849", "4870", "4871":
		return billing.DisputeFraud, true
	case "4863":
		return billing.DisputeUnrecognized, true
	case "4855":
		return billing.DisputeNotReceived, true
	case "4841":
		return billing.DisputeCanceledRecurring, true
	case "4860":
		return billing.DisputeCreditNotProcessed, true
	case "4834":
		return billing.DisputeDuplicate, true
	case "4808":
		return billing.DisputeAuthorization, true
	case "4831", "4842", "4846":
		return billing.DisputeProcessingError, true
	}
	return "", false
}

func amexCode(code string) (billing.DisputeReason, bool) {
	switch code {
	case "F10", "F14", "F24", "F29", "FR2", "FR4", "FR6":
		return billing.DisputeFraud, true
	case "C08":
		return billing.DisputeNotReceived, true
	case "C28":
		return billing.DisputeCanceledRecurring, true
	case "C02", "C04", "C05", "C14":
		return billing.DisputeCreditNotProcessed, true
	case "C31", "C32":
		return billing.DisputeNotAsDescribed, true
	case "P08":
		return billing.DisputeDuplicate, true
	case "A01", "A02", "A08":
		return billing.DisputeAuthorization, true
	}
	return "", false
}

func discoverCode(code string) (billing.DisputeReason, bool) {
	switch code {
	case "UA01", "UA02", "UA05", "UA06", "UA10":
		return billing.DisputeFraud, true
	case "AA":
		return billing.DisputeUnrecognized, true
	case "RG":
		return billing.DisputeNotReceived, true
	case "CD", "RM":
		return billing.DisputeCreditNotProcessed, true
	case "DP":
		return billing.DisputeDuplicate, true
	case "AT":
		return billing.DisputeAuthorization, true
	}
	return "", false
}

// byProviderReason reads Stripe's reason or a rail's own text.
func byProviderReason(reason string) (billing.DisputeReason, bool) {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch r {
	case "":
		return "", false
	case "fraudulent", "debit_not_authorized":
		return billing.DisputeFraud, true
	case "unrecognized":
		return billing.DisputeUnrecognized, true
	case "product_not_received":
		return billing.DisputeNotReceived, true
	case "subscription_canceled":
		return billing.DisputeCanceledRecurring, true
	case "product_unacceptable":
		return billing.DisputeNotAsDescribed, true
	case "credit_not_processed":
		return billing.DisputeCreditNotProcessed, true
	case "duplicate":
		return billing.DisputeDuplicate, true
	case "general", "bank_cannot_process", "check_returned", "customer_initiated", "incorrect_account_details",
		"insufficient_funds", "noncompliant":
		return billing.DisputeOtherReason, true
	}
	switch {
	case strings.Contains(r, "recogni"):
		return billing.DisputeUnrecognized, true
	case strings.Contains(r, "fraud"), strings.Contains(r, "unauthori"), strings.Contains(r, "not authorized"):
		return billing.DisputeFraud, true
	case strings.Contains(r, "not received"), strings.Contains(r, "non-receipt"), strings.Contains(r, "non receipt"):
		return billing.DisputeNotReceived, true
	case strings.Contains(r, "cancel") && (strings.Contains(r, "recurring") || strings.Contains(r, "subscription")):
		return billing.DisputeCanceledRecurring, true
	case strings.Contains(r, "credit not"), strings.Contains(r, "refund"):
		return billing.DisputeCreditNotProcessed, true
	case strings.Contains(r, "duplicate"):
		return billing.DisputeDuplicate, true
	case strings.Contains(r, "not as described"), strings.Contains(r, "defective"):
		return billing.DisputeNotAsDescribed, true
	case strings.Contains(r, "authorization"):
		return billing.DisputeAuthorization, true
	}
	return "", false
}
