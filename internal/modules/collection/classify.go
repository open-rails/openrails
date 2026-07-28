package collection

import (
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/payments"
)

// DeclineCategory classifies a rail decline for collection purposes.
type DeclineCategory int

const (
	// DeclineSoft indicates a transient/recoverable decline (e.g. insufficient
	// funds). Collection keeps retrying on the normal schedule.
	DeclineSoft DeclineCategory = iota
	// DeclineHard indicates a permanent decline (e.g. stolen card, do-not-honor,
	// account closed). Collection must stop immediately; retrying cannot succeed
	// and may flag the merchant with the card networks.
	DeclineHard
)

// hardDeclineNMICodes maps NMI Payment API response_code values that represent
// permanent declines where further retries must stop immediately:
//
//	201 Do not honor                    204 Transaction not allowed
//	220 Incorrect payment information   221 No such card issuer
//	222 No card number on file          223 Expired card
//	224 Invalid expiration date         225 Invalid card security code
//	226 Invalid PIN                     240 Call issuer
//	250 Pick up card                    251 Lost card
//	252 Stolen card                     253 Fraudulent card
//	261 Stop all recurring payments     262 Stop this recurring program
//	263 Update cardholder data          461 Unsupported card type
//
// Soft (retryable) codes fall through to the normal retry schedule:
// 200 generic decline, 202 insufficient funds, 203 over limit, 260/264
// retry-later, 300/400/420/421 gateway/rail/comms, 410/411 merchant config
// (our problem, never the customer's), 430 duplicate, 440/441/460 format.
var hardDeclineNMICodes = map[int]bool{
	201: true, 204: true, 220: true, 221: true, 222: true, 223: true,
	224: true, 225: true, 226: true, 240: true, 250: true, 251: true,
	252: true, 253: true, 261: true, 262: true, 263: true, 461: true,
}

// ClassifyNMIDecline categorizes an NMI Payment API response_code as a hard or
// soft decline. Unknown/zero codes and known transient codes are soft so the
// normal retry schedule applies.
func ClassifyNMIDecline(responseCode int) DeclineCategory {
	if hardDeclineNMICodes[responseCode] {
		return DeclineHard
	}
	return DeclineSoft
}

// RetryableFailureReason reports whether a saved-method charge failure is
// worth retrying on the schedule with the SAME instrument. False means the
// instrument itself needs replacing (expired/invalid card, unknown reason) —
// the schedule pauses instead of burning attempts.
func RetryableFailureReason(rail string, failureCode *string) bool {
	if failureCode == nil {
		return false
	}
	raw := strings.ToLower(strings.TrimSpace(*failureCode))
	switch payments.NormalizeFailureReason(strings.ToLower(strings.TrimSpace(rail)), raw) {
	case payments.FailureInsufficientFunds,
		payments.FailureCardDeclined,
		payments.FailureProcessorError,
		payments.FailureGenericDecline:
		return true
	default:
		return false
	}
}

// Action is what one failed attempt does to the collection schedule.
type Action struct {
	// NextAttemptAt schedules the next attempt (offsets anchored to the FIRST
	// failure). nil with Terminal=false = pause: the instrument needs replacing
	// before collection can resume.
	NextAttemptAt *time.Time
	// Terminal ends the schedule; the consumer applies its own terminal policy
	// (subscription: cancel + revoke; invoice: uncollectible).
	Terminal bool
}

// FailureAction classifies one failed attempt against the cycle's schedule:
// non-retryable reason ⇒ pause; failure budget exhausted ⇒ terminal; else the
// next attempt at firstFailureAt + the next offset.
func FailureAction(cycleHours int, rail string, failureCode *string, priorFailures int, firstFailureAt *time.Time, now time.Time) Action {
	if !RetryableFailureReason(rail, failureCode) {
		return Action{}
	}
	failures := priorFailures + 1
	if failures >= MaxFailures(cycleHours) {
		return Action{Terminal: true}
	}
	first := now
	if firstFailureAt != nil {
		first = *firstFailureAt
	}
	next := first.Add(RetryOffsets(cycleHours)[failures-1])
	return Action{NextAttemptAt: &next}
}
