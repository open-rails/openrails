package collection

import (
	"context"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/modules/payments"
	log "github.com/sirupsen/logrus"
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

	// 250/251 are bucket 2 by owner decision (or#870, 2026-07-29): "treat this as
	// if it were an expired card; same bucket. It's no longer valid but we want
	// the user to re-subscribe before they lose their membership." The instrument
	// is dead, but the CUSTOMER did nothing wrong and a reissued card works — so
	// losing a wallet must not cost a subscription.
	250: DeclineFixPaymentMethod, // pick up card
	251: DeclineFixPaymentMethod, // lost card

	// Bucket 3 — non-recoverable. Cancel at the rail; never touch the stored
	// payment method. 252/253 carry a genuine fraud signal; 261/262 are the
	// issuer withdrawing the recurring mandate outright.
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

// ccbillDeclineOutcomes maps CCBill's own decline vocabulary onto the same
// three buckets. Source: CCBill's published list of their custom alphanumerical
// decline codes (ccbill.com/kb/list-of-credit-card-declined-codes). CCBill does
// not emit ISO codes — it emits BE-nnn, and that string is what arrives
// verbatim in the webhook `failureCode` field.
//
// Keyed on the canonical form (lowercase, dashes stripped), so BE-114, be-114
// and BE114 are one row.
//
// There is deliberately NO bucket-3 row here. The bucket-1 default costs a few
// pointless retries; a wrong bucket-3 row terminally cancels a paying customer.
// BE-112 (No Account) is the one plausible candidate — the Stripe equivalents
// `no_account`/`invalid_account` ARE bucket 3 — and it stays in bucket 1 until
// a live webhook proves the code's meaning on this rail. Widening this is a
// cancellation decision, so it needs cancellation-grade evidence.
//
// Bucket-1 codes, for the record: BE-101 invalid MID/TID (OURS) · BE-105
// invalid transaction · BE-112 no account (see above) · BE-113 insufficient
// funds · BE-119 activity limit exceeded · BE-130 invalid field provided ·
// BE-900..999 system error.
var ccbillDeclineOutcomes = map[string]DeclineOutcome{
	// Bucket 2 — their card, fixable. Charging stops, access and the stored
	// payment method are untouched, the customer is asked to update the card.
	"be102": DeclineFixPaymentMethod, // pickup card — NMI 250's twin, bucket 2 by owner decision
	"be103": DeclineFixPaymentMethod, // do not honor (NMI 201)
	"be107": DeclineFixPaymentMethod, // invalid credit card (NMI 220)
	"be114": DeclineFixPaymentMethod, // expired card (NMI 223)
	"be116": DeclineFixPaymentMethod, // service not allowed (NMI 204)
	"be132": DeclineFixPaymentMethod, // card blocked by CCBill — another instrument is the only fix
	"be146": DeclineFixPaymentMethod, // blocked country — likewise
}

// DeclineCoverage says how much the classifier actually KNEW about a code. The
// outcome alone cannot tell "bucket 1 because the rail says retry" from
// "bucket 1 because nobody ever mapped this code" — and only the second is an
// operational problem worth surfacing.
type DeclineCoverage int

const (
	// CoverageUnrecognized — the rail HAS a decline vocabulary and this code is
	// in none of our tables. Bucket 1 by doctrine, and ALERT-WORTHY: it is
	// either a rail that added a code or a rail we are misreading, and looking
	// is what fixes both. Zero value on purpose, so a forgotten path alerts
	// rather than going quiet.
	CoverageUnrecognized DeclineCoverage = iota
	// CoverageBucketed — an explicit row in one of the bucket tables above.
	CoverageBucketed
	// CoverageKnownRetry — a code the rail's normalized-reason table recognizes
	// that carries no bucket-2/3 meaning. Bucket 1 deliberately; not an alert.
	CoverageKnownRetry
	// CoverageNoVocabulary — nothing to map: no code was recorded, or the rail
	// publishes no decline vocabulary we can bucket at all (Solana's on-chain
	// crank carries no issuer mandate signal; a retired or unknown rail carries
	// nothing). Bucket 1, and silent — alerting on every code of a rail with no
	// table is noise, not signal.
	CoverageNoVocabulary
)

func (c DeclineCoverage) String() string {
	switch c {
	case CoverageBucketed:
		return "bucketed"
	case CoverageKnownRetry:
		return "known_retry"
	case CoverageNoVocabulary:
		return "no_vocabulary"
	default:
		return "unrecognized"
	}
}

// Classification is the classifier's full answer: the bucket, plus how well the
// code was actually understood.
type Classification struct {
	Rail     string
	Code     string
	Outcome  DeclineOutcome
	Coverage DeclineCoverage
}

// NeedsMapping reports an alert-worthy gap: a rail we DO have a vocabulary for
// returned a code none of our tables know.
func (c Classification) NeedsMapping() bool { return c.Coverage == CoverageUnrecognized }

// canonicalCCBillCode normalizes BE-114 / be-114 / BE114 onto one key.
func canonicalCCBillCode(code string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(code) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ClassifyDeclineDetail is ClassifyDecline plus the coverage answer.
func ClassifyDeclineDetail(rail, code string) Classification {
	c := Classification{Rail: strings.ToLower(strings.TrimSpace(rail)), Code: strings.TrimSpace(code)}
	lowered := strings.ToLower(c.Code)
	if lowered == "" {
		// No code recorded is not an unmapped code; there is nothing to map.
		c.Coverage = CoverageNoVocabulary
		return c
	}
	switch c.Rail {
	case "nmi":
		// A custodian-proxied charge lands on the SAME NMI gateway and returns
		// the SAME classic response, so it classifies here too — the rail is
		// nmi either way (or#879: custody is not a rail). #795: one decline
		// vocabulary, two transports.
		if n, err := strconv.Atoi(lowered); err == nil {
			if outcome, ok := nmiDeclineOutcomes[n]; ok {
				c.Outcome, c.Coverage = outcome, CoverageBucketed
				return c
			}
			break
		}
		// The charge path records the verbatim code as the localization id when
		// NMI published one and `nmi_response_<code>` when it did not (#733
		// no-fabrication, nmidirect.FailureCode). Both forms are the SAME
		// evidence and must classify identically — reading only one of them is
		// how the invoice consumer would silently drop every numeric decline
		// into bucket 1.
		if n, err := strconv.Atoi(strings.TrimPrefix(lowered, "nmi_response_")); err == nil {
			if outcome, ok := nmiDeclineOutcomes[n]; ok {
				c.Outcome, c.Coverage = outcome, CoverageBucketed
				return c
			}
			break
		}
		if outcome, ok := nmiDeclineOutcomesByLocalizationID[strings.TrimPrefix(lowered, "nmi_")]; ok {
			c.Outcome, c.Coverage = outcome, CoverageBucketed
			return c
		}
	case "stripe":
		if outcome, ok := stripeDeclineOutcomes[lowered]; ok {
			c.Outcome, c.Coverage = outcome, CoverageBucketed
			return c
		}
	case "ccbill":
		if outcome, ok := ccbillDeclineOutcomes[canonicalCCBillCode(lowered)]; ok {
			c.Outcome, c.Coverage = outcome, CoverageBucketed
			return c
		}
	default:
		// Solana's on-chain crank vocabulary carries no issuer mandate signal at
		// all, and `vaulted_card` is a RETIRED value (or#879) that must never
		// carry a taxonomy again. Both classify bucket 1 for every code:
		// OpenRails terminates only on provider-confirmed truth or genuinely
		// exhausted attempts, never on a decline string it cannot read.
		c.Coverage = CoverageNoVocabulary
		return c
	}
	// Bucket 1 either way; the only question left is whether the rail's own
	// normalized-reason table recognizes the code. If it does, bucket 1 is a
	// decision. If it does not, nobody has ever mapped this code.
	if payments.NormalizeFailureReason(c.Rail, lowered) != payments.FailureUnknown {
		c.Coverage = CoverageKnownRetry
	}
	return c
}

// ClassifyDecline is the ONE answer to "what does this decline mean" for any
// rail. code is the failure code recorded VERBATIM off the rail (NMI: the
// numeric response_code or its localization id; Stripe: decline_code; CCBill:
// the BE-nnn failureCode).
//
// Empty and unrecognized codes are DeclineRetry — no evidence, no action.
func ClassifyDecline(rail, code string) DeclineOutcome {
	return ClassifyDeclineDetail(rail, code).Outcome
}

// AlertUnmappedDecline emits the ONE loud, greppable line for a decline code no
// table recognizes. Loud because the safe default is SILENT: an unmapped code
// keeps dunning exactly like an ordinary insufficient-funds decline, so nothing
// downstream ever looks wrong — a rail could add a "stop all recurring" code
// and we would retry it into the ground forever without a single symptom.
// Error level, not warn: the doctrine's correctness rests on these tables being
// complete, and completeness is only observable here.
func AlertUnmappedDecline(ctx context.Context, c Classification) {
	if !c.NeedsMapping() {
		return
	}
	log.WithContext(ctx).WithFields(log.Fields{
		"rail":            c.Rail,
		"failure_code":    c.Code,
		"decline_outcome": c.Outcome.String(),
	}).Error("or#870: UNMAPPED decline code — no bucket-table row and no normalized reason for this rail; treated as bucket 1 (retry) by doctrine. Map it in internal/modules/collection/decline.go")
}

// ClassifyNMIResponseCode is ClassifyDecline for a numeric NMI response code
// already parsed off gateway evidence. 0 (no code recorded) is DeclineRetry.
func ClassifyNMIResponseCode(responseCode int) DeclineOutcome {
	if responseCode == 0 {
		return DeclineRetry
	}
	return nmiDeclineOutcomes[responseCode]
}
