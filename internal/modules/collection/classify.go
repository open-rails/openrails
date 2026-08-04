package collection

import (
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/payments"
)

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
