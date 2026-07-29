package collection

import (
	"strconv"
	"strings"
)

// or#870 decline doctrine — ONE classifier, three outcomes.
//
// The standing rule above everything else: OpenRails NEVER deletes a stored
// payment method. Not on expiry, not on a stolen card, not on cancellation, not
// ever. Only the end user deletes their own instrument. When a subscription is
// genuinely non-recoverable we cancel it AT THE RAIL and leave the stored
// payment method untouched, so the customer can update or remove it themselves.
//
// This collapses the two overlapping classifiers that preceded it —
// ClassifyNMIDecline (18 "hard" codes: stop retrying) and IsNonRetryableDecline
// (6 "certainty" codes: safe to cancel). They were never contradictory, they
// answered different questions, but nothing stated the policy so the gap
// between them — the 12 codes that were "hard" but not "certain" — was
// undefined behaviour. Now every code has exactly one answer.

// DeclineOutcome is what a rail decline means for collection.
type DeclineOutcome int

const (
	// DeclineRetry — OURS or transient. Gateway/rail/comms failures, our own
	// merchant misconfiguration, duplicates, format errors, and the ordinary
	// money-shaped declines that may well succeed next cycle. Keep the dunning
	// schedule; tell the customer we are still trying, and tell them again when
	// the schedule is exhausted.
	//
	// This is the ZERO VALUE on purpose: an unrecognized code is bucket 1,
	// never 2 or 3. Missing evidence must never cost a customer their
	// subscription.
	DeclineRetry DeclineOutcome = iota

	// DeclineFixPaymentMethod — THEIR card, fixable. Retrying burns attempts
	// against an instrument that cannot succeed, annoys the issuer and risks
	// fraud scoring — but the customer can fix it in a minute. Stop charging
	// immediately, KEEP the subscription and its entitlements (the same grace a
	// manually-dunned customer gets), and ask them to update the card. An
	// expired card is the single most recoverable failure in billing.
	DeclineFixPaymentMethod

	// DeclineNonRecoverable — the issuer has withdrawn the recurring mandate or
	// the instrument is permanently dead. Cancel the subscription AT THE RAIL,
	// touch NOTHING about the stored payment method, and tell the customer they
	// are welcome to re-subscribe.
	DeclineNonRecoverable
)

func (o DeclineOutcome) String() string {
	switch o {
	case DeclineFixPaymentMethod:
		return "fix_payment_method"
	case DeclineNonRecoverable:
		return "non_recoverable"
	default:
		return "retry"
	}
}

// StopsCharging reports whether this outcome ends charge attempts against the
// instrument. True for buckets 2 and 3 — for opposite reasons.
func (o DeclineOutcome) StopsCharging() bool { return o != DeclineRetry }

// nmiDeclineOutcomes is THE table. NMI has the richest published code set, so
// the doctrine is written against it and the other rails map onto the same
// three buckets below.
//
// Only non-retry rows are listed: absence means DeclineRetry (bucket 1), which
// is also the zero value, so an unrecognized code can only ever land in bucket
// 1. The bucket-1 codes NMI does publish, for the record:
//
//	200 generic decline · 202 insufficient funds · 203 over limit ·
//	260/264 retry-later · 300/400/420/421 gateway/rail/comms ·
//	410/411 merchant config (OURS, never the customer's) · 430 duplicate ·
//	440/441/460 format
//
// Widening bucket 3 widens the set of customers OpenRails terminally cancels,
// so each addition needs the scrutiny of a cancellation.
var nmiDeclineOutcomes = map[int]DeclineOutcome{
	// Bucket 2 — their card, fixable. Stop charging, keep access, ask them to
	// update the payment method.
	201: DeclineFixPaymentMethod, // do not honor
	204: DeclineFixPaymentMethod, // transaction not allowed
	220: DeclineFixPaymentMethod, // incorrect payment information
	221: DeclineFixPaymentMethod, // no such card issuer
	222: DeclineFixPaymentMethod, // no card number on file with issuer
	223: DeclineFixPaymentMethod, // expired card
	224: DeclineFixPaymentMethod, // invalid expiration date
	225: DeclineFixPaymentMethod, // invalid card security code
	226: DeclineFixPaymentMethod, // invalid PIN
	240: DeclineFixPaymentMethod, // call issuer for further information
	263: DeclineFixPaymentMethod, // declined — update cardholder data
	461: DeclineFixPaymentMethod, // unsupported card type

	// Bucket 3 — non-recoverable. Cancel at the rail; never touch the stored
	// payment method.
	//
	// OPEN CALL (or#870, unresolved at implementation time): 250/251 (pick-up /
	// lost) could argue for bucket 2 instead — the card is dead but the customer
	// is not a fraudster, and bucket 2 keeps their access while they add a
	// different card. 252/253 carry a genuine fraud signal and belong here.
	// Filed as bucket 3; flipping 250/251 is a two-line change to this table.
	250: DeclineNonRecoverable, // pick up card
	251: DeclineNonRecoverable, // lost card
	252: DeclineNonRecoverable, // stolen card
	253: DeclineNonRecoverable, // fraudulent card
	261: DeclineNonRecoverable, // declined — stop all recurring payments
	262: DeclineNonRecoverable, // declined — stop this recurring program
}

// nmiDeclineLocalizationIDs is NMI's own code→localization-id vocabulary for
// the non-retry rows (the payments mirror records whichever form the rail
// returned). Derived from internal/integrations/nmi's published table; only the
// rows nmiDeclineOutcomes names need to appear, because everything else is
// bucket 1 either way.
var nmiDeclineLocalizationIDs = map[int]string{
	201: "do_not_honor",
	204: "transaction_not_allowed",
	220: "incorrect_payment_information",
	221: "no_such_card_issuer",
	222: "no_card_number_on_file_with_issuer",
	223: "expired_card",
	224: "invalid_expiration_date",
	225: "invalid_card_security_code",
	226: "invalid_pin",
	240: "call_issuer_for_further_information",
	250: "pick_up_card",
	251: "lost_card",
	252: "stolen_card",
	253: "fraudulent_card",
	261: "declined_stop_all_recurring_payments",
	262: "declined_stop_this_recurring_program",
	263: "declined_update_cardholder_data_available",
	461: "unsupported_card_type",
}

// nmiDeclineOutcomesByLocalizationID is nmiDeclineOutcomes keyed by NMI's
// localization id — DERIVED, so the two forms of the same code can never drift.
var nmiDeclineOutcomesByLocalizationID = func() map[string]DeclineOutcome {
	m := make(map[string]DeclineOutcome, len(nmiDeclineOutcomes))
	for code, outcome := range nmiDeclineOutcomes {
		if id := nmiDeclineLocalizationIDs[code]; id != "" {
			m[id] = outcome
		}
	}
	return m
}()

// stripeDeclineOutcomes maps Stripe's decline_code vocabulary onto the same
// three buckets. Absent ⇒ bucket 1, so insufficient_funds, generic_decline,
// card_declined, processing_error, issuer_not_available, try_again_later,
// duplicate_transaction, card_velocity_exceeded and
// withdrawal_count_limit_exceeded all keep the dunning schedule.
//
// The bucket-3 rows are EXACTLY the pre-or#870 non-retryable set — this
// doctrine reorganizes the answers, it does not widen who gets cancelled.
var stripeDeclineOutcomes = map[string]DeclineOutcome{
	// Bucket 2 — their card, fixable.
	"expired_card":            DeclineFixPaymentMethod,
	"invalid_expiry_month":    DeclineFixPaymentMethod,
	"invalid_expiry_year":     DeclineFixPaymentMethod,
	"incorrect_cvc":           DeclineFixPaymentMethod,
	"invalid_cvc":             DeclineFixPaymentMethod,
	"incorrect_zip":           DeclineFixPaymentMethod,
	"incorrect_number":        DeclineFixPaymentMethod,
	"invalid_number":          DeclineFixPaymentMethod,
	"incorrect_pin":           DeclineFixPaymentMethod,
	"invalid_pin":             DeclineFixPaymentMethod,
	"pin_try_exceeded":        DeclineFixPaymentMethod,
	"card_not_supported":      DeclineFixPaymentMethod,
	"currency_not_supported":  DeclineFixPaymentMethod,
	"do_not_honor":            DeclineFixPaymentMethod,
	"transaction_not_allowed": DeclineFixPaymentMethod,
	"call_issuer":             DeclineFixPaymentMethod,
	"authentication_required": DeclineFixPaymentMethod,

	// Bucket 3 — non-recoverable.
	"revocation_of_authorization":      DeclineNonRecoverable,
	"revocation_of_all_authorizations": DeclineNonRecoverable,
	"stop_payment_order":               DeclineNonRecoverable,
	"lost_card":                        DeclineNonRecoverable,
	"stolen_card":                      DeclineNonRecoverable,
	"pickup_card":                      DeclineNonRecoverable,
	"fraudulent":                       DeclineNonRecoverable,
	"invalid_account":                  DeclineNonRecoverable,
	"no_account":                       DeclineNonRecoverable,
}

// ClassifyDecline is the ONE answer to "what does this decline mean" for any
// rail. code is the failure code recorded VERBATIM off the rail (NMI: the
// numeric response_code or its localization id; Stripe: decline_code).
//
// Empty and unrecognized codes are DeclineRetry — no evidence, no action.
func ClassifyDecline(rail, code string) DeclineOutcome {
	code = strings.ToLower(strings.TrimSpace(code))
	if code == "" {
		return DeclineRetry
	}
	switch strings.ToLower(strings.TrimSpace(rail)) {
	case "nmi", "mobius", "vaulted_card":
		// vaulted_card parses NMI classic responses through the same taxonomy
		// (#795: one decline vocabulary, two transports).
		if n, err := strconv.Atoi(code); err == nil {
			return nmiDeclineOutcomes[n]
		}
		return nmiDeclineOutcomesByLocalizationID[strings.TrimPrefix(code, "nmi_")]
	case "stripe":
		return stripeDeclineOutcomes[code]
	default:
		// CCBill publishes no enumerated decline vocabulary (its failureCode
		// values are recorded verbatim but unmapped — see
		// payments.ccbillFailureReasons, deliberately empty), and Solana's
		// on-chain crank vocabulary carries no issuer mandate signal at all.
		// Both therefore classify as bucket 1 for every code: OpenRails keeps
		// dunning on the schedule and terminates only on provider-confirmed
		// truth or genuinely exhausted attempts, never on a decline string it
		// cannot read. Widening either needs live-verified codes first.
		return DeclineRetry
	}
}

// ClassifyNMIResponseCode is ClassifyDecline for a numeric NMI response code
// already parsed off gateway evidence. 0 (no code recorded) is DeclineRetry.
func ClassifyNMIResponseCode(responseCode int) DeclineOutcome {
	if responseCode == 0 {
		return DeclineRetry
	}
	return nmiDeclineOutcomes[responseCode]
}
