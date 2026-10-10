package collection

import (
	"time"

	"github.com/open-rails/openrails/internal/decline"
)

// Action is what ONE failed collection attempt does to the schedule. Every
// Action names exactly one disposition; bucket 2 is a deliberate stop awaiting
// a new payment method, never a state nothing resolves.
type Action struct {
	// Decline is the classifier's answer. Its Action retries (keeps the
	// schedule) or stops charging, for opposite reasons.
	Decline decline.Result
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
	return a.Decline.Action == decline.FixPaymentMethod
}

// ScheduleExhausted distinguishes the two roads to Terminal: bucket 1 that ran
// out of attempts (we gave up) versus bucket 3, terminal on the first look
// (the issuer withdrew the mandate).
func (a Action) ScheduleExhausted() bool {
	return a.Terminal && !a.Decline.Action.StopsCharging()
}

// FailureAction is the decision for one failed attempt: classify the decline
// into three buckets, then apply the cycle's schedule to bucket 1.
//
// cycleHours is the real billing cycle of what is collected, never a hardcoded
// month. failureCode is the rail's code verbatim; nil/empty is bucket 1 (no
// evidence is not evidence). An unknown cycle refuses bucket 1 with
// ErrUnknownCycle; buckets 2 and 3 do not depend on the cycle.
func FailureAction(cycleHours int, rail string, failureCode *string, priorFailures int, firstFailureAt *time.Time, now time.Time) (Action, error) {
	code := ""
	if failureCode != nil {
		code = *failureCode
	}
	d := decline.Classify(rail, code)
	switch d.Action {
	case decline.NonRecoverable:
		// Bucket 3: the issuer withdrew the recurring mandate or the
		// instrument is dead. Terminal on the first look.
		return Action{Decline: d, Terminal: true}, nil
	case decline.FixPaymentMethod:
		// Bucket 2: the customer's fixable card. Stop charging, terminate nothing.
		return Action{Decline: d}, nil
	}

	// Bucket 1 — ours or transient. Keep the schedule.
	offsets, err := RetryOffsets(cycleHours)
	if err != nil {
		return Action{}, err
	}
	failures := priorFailures + 1
	if failures >= len(offsets)+1 {
		return Action{Decline: d, Terminal: true}, nil
	}
	first := now
	if firstFailureAt != nil {
		first = *firstFailureAt
	}
	next := first.Add(offsets[failures-1])
	return Action{Decline: d, NextAttemptAt: &next}, nil
}
