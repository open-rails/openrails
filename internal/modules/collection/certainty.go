package collection

import (
	"strconv"
	"strings"
)

// The certainty legs that may justify a TERMINAL collection outcome — a local
// cancel + entitlement revocation, and the IRREVERSIBLE provider-side delete
// it queues (#664/#821/#839/#840).
//
// A terminal outcome is only ever reached through one of these. Explicitly NOT
// certainty: a date comparison (NMI rebills forever — a lapsed
// next_billing_date is the NORMAL state of every dunning customer), a
// zero-length dunning window, or the absence of one of OUR OWN rows (a
// vault-sync bug is indistinguishable from a dead card). Those PARK as
// `unknown` and a targeted provider probe resolves them.
//
// These constants live here — the collection engine — because BOTH consumers
// need them: the reconcile decider (which depends on collection) and the
// subscription lifecycle's FailMembership chokepoint.
const (
	// CertaintyProviderConfirmedDead: the provider itself says the schedule is
	// gone (roster cancelled/expired, or absent from a PROVEN-exhaustive
	// roster). Mirroring provider truth, not our inference.
	CertaintyProviderConfirmedDead = "provider_confirmed_dead"
	// CertaintyNonRetryableDecline: a recorded decline whose rail code means
	// the billing authorization is withdrawn / the account cannot be charged
	// again. Retryable and unrecognized codes never qualify.
	CertaintyNonRetryableDecline = "non_retryable_decline"
	// CertaintyDunningExhausted: real recorded dunning ATTEMPTS reached the
	// policy max. Never "grace elapsed", never "the date is old", and never an
	// attempt we declined to make because our own data was missing.
	CertaintyDunningExhausted = "dunning_exhausted"
	// CertaintyHardDecline: a real gateway decline the dunning classifier calls
	// permanent (ClassifyNMIDecline ⇒ DeclineHard) but that is WIDER than the
	// non-retryable set above — e.g. 223 expired card, 201 do-not-honor. It is
	// still first-party provider evidence of a real charge attempt, so it names
	// its own leg rather than borrowing a stronger one it has not earned.
	CertaintyHardDecline = "hard_decline"
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
	case "nmi", "mobius", "vaulted_card":
		if n, err := strconv.Atoi(code); err == nil {
			return nmiNonRetryableResponseCodes[n]
		}
		return nmiNonRetryableLocalizationIDs[strings.TrimPrefix(code, "nmi_")]
	case "stripe":
		return stripeNonRetryableDeclineCodes[code]
	default:
		// CCBill/Solana expose no decline vocabulary this classifier trusts.
		return false
	}
}
