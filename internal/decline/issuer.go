package decline

import (
	"slices"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// issuerCodes are the ISO 8583 response codes Visa and Mastercard share, as
// the issuer sent them (NMI processor_response_code, Stripe
// network_decline_code). One refines a rail reason that names no cause.
var issuerCodes = map[string]billing.DeclineReason{
	"01": billing.DeclineCallIssuer, "02": billing.DeclineCallIssuer,
	"03": billing.DeclineMerchantConfig, "58": billing.DeclineMerchantConfig,
	"04": billing.DeclinePickupCard, "07": billing.DeclinePickupCard,
	"05": billing.DeclineDoNotHonor,
	"12": billing.DeclineTransactionNotAllowed, "57": billing.DeclineTransactionNotAllowed, "93": billing.DeclineTransactionNotAllowed,
	"13": billing.DeclineInvalidRequest,
	"14": billing.DeclineIncorrectNumber,
	"15": billing.DeclineNoSuchIssuer,
	"19": billing.DeclineTryAgainLater,
	"41": billing.DeclineLostCard,
	"43": billing.DeclineStolenCard,
	"46": billing.DeclineInvalidAccount, "78": billing.DeclineInvalidAccount, "39": billing.DeclineInvalidAccount,
	"52": billing.DeclineInvalidAccount, "53": billing.DeclineInvalidAccount,
	"51": billing.DeclineInsufficientFunds,
	"54": billing.DeclineExpiredCard,
	"55": billing.DeclineInvalidPIN, "75": billing.DeclineInvalidPIN,
	"59": billing.DeclineSuspectedFraud,
	"61": billing.DeclineOverLimit, "65": billing.DeclineOverLimit,
	"62": billing.DeclineRestrictedCard,
	"63": billing.DeclineSecurityViolation,
	"82": billing.DeclineIncorrectCVC, "N7": billing.DeclineIncorrectCVC,
	"1A": billing.DeclineAuthenticationRequired,
	"91": billing.DeclineIssuerUnavailable,
	"94": billing.DeclineDuplicateTransaction,
	"96": billing.DeclineProcessingError,
	"R0": billing.DeclineStopRecurring, "R1": billing.DeclineStopRecurring, "R3": billing.DeclineStopRecurring,
}

// visaCategories are Visa's decline categories 1 to 3; every other code is 4.
var visaCategories = func() map[string]int {
	m := map[string]int{}
	for category, codes := range map[int]string{
		1: "04 07 12 14 15 41 43 46 57 R0 R1 R3",
		2: "03 19 39 51 52 53 59 61 62 65 75 78 86 91 93 96 N3 N4 Z5 5C 9G",
		3: "54 55 63 82 N7 1A 70 6P",
	} {
		for _, code := range strings.Fields(codes) {
			m[code] = category
		}
	}
	return m
}()

// VisaCategoryCodes are the issuer codes of Visa's categories 1 to 3; every
// other code is 4.
func VisaCategoryCodes() map[int][]string {
	out := map[int][]string{}
	for code, category := range visaCategories {
		out[category] = append(out[category], code)
	}
	for _, codes := range out {
		slices.Sort(codes)
	}
	return out
}

// VisaCategory is Visa's category of an issuer response code: 1 never
// approve, 2 cannot approve now, 3 fix the data, 4 any other; 0 without a
// code.
func VisaCategory(code string) int {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return 0
	}
	if c, ok := visaCategories[code]; ok {
		return c
	}
	return 4
}

// unspecific is a rail reason that names no cause: the issuer's own code may
// say more.
func unspecific(reason billing.DeclineReason) bool {
	return reason == billing.DeclineGeneric || reason == billing.DeclineDoNotHonor || reason == billing.DeclineUnknown
}

// issued refines an unspecific reason by the issuer's code. A refinement
// never cancels: its action stops at waiting for a new card.
func (r Result) issued(code string) Result {
	reason, ok := issuerCodes[strings.ToUpper(strings.TrimSpace(code))]
	if !ok || !unspecific(r.Reason) || reason == r.Reason {
		return r
	}
	r = r.with(reason)
	if r.Action == NonRecoverable {
		r.Action = FixPaymentMethod
	}
	return r
}
