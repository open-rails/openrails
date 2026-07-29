package collection

import (
	"time"
)

// Action is what ONE failed collection attempt does to the schedule.
//
// Every Action names exactly one disposition. There is deliberately no
// "neither retry nor terminal" return: the pre-doctrine FailureAction returned
// a bare Action{} for any code its two-way split called non-retryable — no
// next attempt AND not terminal — so the row parked in a state nothing
// resolved (or#828). Under the or#870 doctrine that case is bucket 2, a
// DELIBERATE stop with a reason, a notification and a resume path, not a hole.
type Action struct {
	// Outcome is the or#870 bucket this decline fell in. Bucket 1 (the zero
	// value) keeps the schedule; buckets 2 and 3 stop charging, for opposite
	// reasons.
	Outcome DeclineOutcome
	// NextAttemptAt schedules the next attempt (offsets anchored to the FIRST
	// failure). Set only for bucket 1 while the cycle's schedule has attempts
	// left.
	NextAttemptAt *time.Time
	// Terminal ends the schedule; the consumer applies its own terminal policy
	// (subscription: cancel at the rail + revoke; invoice: uncollectible). Set
	// for bucket 3 immediately, and for bucket 1 once the schedule is spent.
	Terminal bool
}

// AwaitingPaymentMethod is bucket 2: charging STOPS but nothing is terminated.
// The debt (or subscription) stands, access is untouched, and the customer
// fixing the instrument is what resumes collection.
func (a Action) AwaitingPaymentMethod() bool {
	return a.Outcome == DeclineFixPaymentMethod
}

// ScheduleExhausted distinguishes the two roads to Terminal: bucket 1 that ran
// out of attempts (we gave up) versus bucket 3, terminal on the first look
// (the issuer withdrew the mandate).
func (a Action) ScheduleExhausted() bool {
	return a.Terminal && a.Outcome == DeclineRetry
}

// FailureAction is the ONE decision for one failed attempt, shared by both
// consumers: classify the decline with the or#870 three-bucket doctrine, then
// apply the cycle's schedule to bucket 1.
//
// cycleHours is the REAL billing cycle of the thing being collected — the
// subscription's price cadence (BillingCycleHoursOf) or the invoice's
// statement period (CycleHoursBetween) — never a hardcoded month (or#828).
//
// failureCode is the code recorded VERBATIM off the rail; nil/empty is bucket
// 1, because no evidence is not evidence.
func FailureAction(cycleHours int, rail string, failureCode *string, priorFailures int, firstFailureAt *time.Time, now time.Time) Action {
	code := ""
	if failureCode != nil {
		code = *failureCode
	}
	switch outcome := ClassifyDecline(rail, code); outcome {
	case DeclineNonRecoverable:
		// Bucket 3 — the issuer withdrew the recurring mandate, or the
		// instrument is permanently dead. Terminal on the FIRST look: there is
		// no schedule worth running against an instrument that cannot succeed.
		return Action{Outcome: outcome, Terminal: true}
	case DeclineFixPaymentMethod:
		// Bucket 2 — their card, fixable in a minute. Stop charging NOW, and
		// terminate NOTHING.
		return Action{Outcome: outcome}
	}

	// Bucket 1 — ours or transient. Keep the schedule.
	failures := priorFailures + 1
	if failures >= MaxFailures(cycleHours) {
		return Action{Outcome: DeclineRetry, Terminal: true}
	}
	first := now
	if firstFailureAt != nil {
		first = *firstFailureAt
	}
	next := first.Add(RetryOffsets(cycleHours)[failures-1])
	return Action{Outcome: DeclineRetry, NextAttemptAt: &next}
}
