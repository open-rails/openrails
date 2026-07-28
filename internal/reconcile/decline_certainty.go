package reconcile

import (
	"strconv"
	"strings"
)

// #821 certainty leg: which rail decline codes mean the billing authorization
// is GONE, as opposed to "this attempt failed and the rail will try again".
//
// Doctrine: cancellation is a last resort, so this classifier is deliberately
// TIGHT. Only codes whose plain meaning is "never charge this again" qualify;
// insufficient funds, do-not-honor, expiry, comms errors, gateway rejects and
// EVERY unrecognized code are retryable — no evidence, no action. Widening this
// set widens the set of customers OpenRails will terminally cancel and delete
// at the provider, so each addition needs the same scrutiny as a delete.

// nmiNonRetryableResponseCodes are the NMI/Mobius gateway response codes that
// withdraw the recurring authorization outright (261/262), or that mean the
// instrument itself is permanently unusable (250-253 pick-up/lost/stolen/
// fraudulent). 264 ("retry in a few days") is explicitly retryable; 202/201/
// 203/223 are ordinary dunning declines.
var nmiNonRetryableResponseCodes = map[int]bool{
	250: true, // pick_up_card
	251: true, // lost_card
	252: true, // stolen_card
	253: true, // fraudulent_card
	261: true, // declined_stop_all_recurring_payments
	262: true, // declined_stop_this_recurring_program
}

// nmiNonRetryableLocalizationIDs is the same set keyed by the localization id,
// because the mirror records whichever form the rail returned.
var nmiNonRetryableLocalizationIDs = map[string]bool{
	"pick_up_card":                         true,
	"lost_card":                            true,
	"stolen_card":                          true,
	"fraudulent_card":                      true,
	"declined_stop_all_recurring_payments": true,
	"declined_stop_this_recurring_program": true,
}

// stripeNonRetryableDeclineCodes: Stripe decline_codes that revoke the mandate
// or mean the account no longer exists.
var stripeNonRetryableDeclineCodes = map[string]bool{
	"revocation_of_authorization":      true,
	"revocation_of_all_authorizations": true,
	"stop_payment_order":               true,
	"lost_card":                        true,
	"stolen_card":                      true,
	"pickup_card":                      true,
	"fraudulent":                       true,
	"invalid_account":                  true,
	"no_account":                       true,
}

// IsNonRetryableDecline reports whether a rail decline code is CERTAINTY that
// the subscription can never be billed again — the #664/#821 certainty leg.
// Unrecognized codes are retryable by construction: an unknown code is missing
// evidence, and missing evidence never cancels.
func IsNonRetryableDecline(rail, code string) bool {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case string(ProviderNMI), "mobius", "vaulted_card":
		if n, err := strconv.Atoi(code); err == nil {
			return nmiNonRetryableResponseCodes[n]
		}
		return nmiNonRetryableLocalizationIDs[strings.TrimPrefix(code, "nmi_")]
	case string(ProviderStripe):
		return stripeNonRetryableDeclineCodes[code]
	default:
		// CCBill/Solana expose no decline vocabulary this classifier trusts.
		return false
	}
}
