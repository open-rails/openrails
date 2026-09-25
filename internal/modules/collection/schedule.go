// Package collection is the ONE collection engine core (#828): "a user owes
// money; collect it on a schedule; classify outcomes; verify ambiguity; go
// terminal deliberately." Two consumers ride it — subscription rebill dunning
// (terminal ⇒ cancel + entitlement revoke, staleness ⇒ cancel-never-charge)
// and invoice arrears collection (terminal ⇒ uncollectible). Consumer POLICY
// stays at the consumer; the schedule table, outcome classification and
// failure-action mechanics live here, once.
package collection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// Cadence-relative retry schedule (#359, extracted by #828).
//
// The schedule is a HARDCODED function of the billing cycle — no knob.
// Each entry is a retry moment as an OFFSET from the INITIAL failure:
//
//	cycle < 4 days        -> no retries (the first failure is terminal)
//	4 days <= cycle < 28  -> +1d, +2d                  ("weekly": 3 failures total)
//	cycle >= 28 days      -> +2d, +5d, +9d, +13d       ("monthly": 5 failures total)
//	unknown (<= 0)        -> ErrUnknownCycle: nothing is scheduled or charged
//
// Boundary rationale: the derived staleness window (last offset + slack) must
// fit WELL INSIDE one billing cycle, so a subscription is never still dunning
// the old period when the next charge is due.
const (
	// MinRetryCycleHours is the shortest billing cycle that gets any retries at
	// all; below it the first failure is terminal. 96h (4 days).
	MinRetryCycleHours = 4 * 24
	// MonthlyCycleHours is where the monthly (capped) tier starts. 672h (28 days).
	MonthlyCycleHours = 28 * 24
	// maxWindowSlack is the most slack added past the last retry offset: 24h
	// tolerates a late worker run. Short cycles get half their cycle instead
	// (windowSlack), so the window always ends inside the cycle.
	maxWindowSlack = 24 * time.Hour
)

// ErrUnknownCycle is a collection asked to schedule without a billing cycle.
// Consumers fail closed: nothing is charged, retried or terminated on a
// guessed cadence, and an operator finding (FindingUnknownCycle) is raised.
var ErrUnknownCycle = errors.New("billing cycle is unknown")

// UnknownCycleError carries the offending cycle; it matches ErrUnknownCycle.
type UnknownCycleError struct{ CycleHours int }

func (e *UnknownCycleError) Error() string {
	return fmt.Sprintf("%s (cycle %dh)", ErrUnknownCycle, e.CycleHours)
}

func (e *UnknownCycleError) Is(target error) bool { return target == ErrUnknownCycle }

// FindingUnknownCycle is a collection that refused to run for want of a cycle.
const FindingUnknownCycle = "life.cadence.unknown"

type findingWriter interface {
	UpsertReconciliationFinding(context.Context, gen.UpsertReconciliationFindingParams) (gen.OpenrailsReconciliationFinding, error)
}

// RecordUnknownCycle raises the operator finding for subject (a subscription
// or invoice id) that collection refused because its cycle is unknown.
func RecordUnknownCycle(ctx context.Context, q findingWriter, merchantID uuid.UUID, subject, consumer string, cause error) error {
	evidence, _ := json.Marshal(map[string]any{"subject": subject, "consumer": consumer, "error": cause.Error()})
	action := fmt.Sprintf("%s %s has no billing cycle, so collection refuses to charge, retry or end it (%v). Give its price a recurring cadence or resolve it by hand", consumer, subject, cause)
	_, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: merchantID, FindingType: FindingUnknownCycle, SubjectKey: subject,
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
	})
	return err
}

var (
	// offsetsWeekly: 2 retries, one day apart.
	offsetsWeekly = []time.Duration{24 * time.Hour, 48 * time.Hour}
	// offsetsMonthly: progressive +2d, +5d, +9d, +13d (gaps 2,3,4,4).
	offsetsMonthly = []time.Duration{
		2 * 24 * time.Hour,
		5 * 24 * time.Hour,
		9 * 24 * time.Hour,
		13 * 24 * time.Hour,
	}
)

// RetryOffsets returns the hardcoded retry schedule for a billing cycle in
// HOURS. An empty schedule means no retries — the first failure is terminal.
// Callers must not mutate the returned slice.
func RetryOffsets(cycleHours int) ([]time.Duration, error) {
	return DefaultPolicy.RetryOffsets(cycleHours)
}

// MaxFailures returns how many consecutive failures (counting the initial
// one) a billing cycle tolerates before collection goes terminal.
func MaxFailures(cycleHours int) (int, error) {
	return DefaultPolicy.MaxFailures(cycleHours)
}

// NextRetryIn returns how long after the failures-th consecutive failure
// (1-based) the next retry should run, or 0 when that failure is terminal.
// The gaps reproduce the offset schedule when each retry runs on time and
// degrade gracefully when the worker is late — the next retry is always
// relative to the failure that just happened, never in the past.
func NextRetryIn(cycleHours, failures int) (time.Duration, error) {
	return DefaultPolicy.NextRetryIn(cycleHours, failures)
}

// NextAttemptAt is the schedule's next attempt after the failures-th
// consecutive failure (1-based) made at lastAttempt. ok is false when that
// failure spent the schedule. Every consumer takes retry times from here.
func NextAttemptAt(cycleHours, failures int, lastAttempt time.Time) (next time.Time, ok bool, err error) {
	return DefaultPolicy.NextAttemptAt(cycleHours, failures, lastAttempt)
}

// Window returns the DERIVED staleness window (#344, #359): how long past the
// missed charge collection may still attempt one. Past it the charge is SKIPPED
// — a card that failed months ago is never surprise-charged by a catch-up run —
// and the row parks for provider verification. Expiry is NOT a terminal
// outcome (#839): a clock reading is not evidence a subscription is dead.
//
// window = last retry offset + min(24h, cycle/2). A 0-retry cycle (sub-4-day
// cadence) has no offsets, so its window is the slack alone — NOT zero (#839),
// and never a whole cycle: an hourly membership gets 30 minutes, a daily one
// 12 hours. Window(cycle) < cycle for every known cycle.
func Window(cycleHours int) (time.Duration, error) {
	return DefaultPolicy.Window(cycleHours)
}

func windowSlack(cycle time.Duration) time.Duration {
	return min(maxWindowSlack, cycle/2)
}

// BillingCycleHoursOf returns the price's billing cycle in HOURS, or 0 when
// the price or its cycle is unknown (one-time prices), which the schedule
// functions refuse with ErrUnknownCycle.
func BillingCycleHoursOf(price *models.Price) int {
	if price == nil {
		return 0
	}
	cycleHours := price.RecurringCycleHours()
	if cycleHours == nil {
		return 0
	}
	return *cycleHours
}

// CycleHoursBetween returns a billing period's length in HOURS — the invoice
// consumer's analogue of BillingCycleHoursOf. An invoice's REAL cycle is its
// statement period (period_from → period_to), so a weekly statement is dunned
// on the weekly offsets and an annual one on the monthly offsets, instead of
// every invoice being dunned on a hardcoded month (or#828).
//
// A degenerate or unset period returns 0 = unknown (ErrUnknownCycle).
func CycleHoursBetween(from, to time.Time) int {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return 0
	}
	return int(to.Sub(from) / time.Hour)
}

// TransientLadder is the short retry ladder for explicit try-again answers:
// minutes after the attempt, bounded, and not counted as dunning failures.
// After it, the answer counts as an ordinary decline on the cycle schedule.
var TransientLadder = []time.Duration{5 * time.Minute, 30 * time.Minute}

// NextTransientAttempt is when the used+1-th transient retry runs, or false
// when the ladder is spent.
func NextTransientAttempt(used int, at time.Time) (time.Time, bool) {
	return DefaultPolicy.NextTransientAttempt(used, at)
}
