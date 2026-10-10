// Package collection is the collection engine core: schedule retries for money
// owed, classify outcomes and go terminal deliberately. Subscription dunning and
// invoice arrears collection consume it; their terminal policy stays with them.
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

// Default retry schedule, as offsets from the initial failure:
//
//	cycle < 4 days        -> no retries (the first failure is terminal)
//	4 days <= cycle < 28  -> +1d, +2d                  (3 failures total)
//	cycle >= 28 days      -> +2d, +5d, +9d, +13d       (5 failures total)
//	unknown (<= 0)        -> ErrUnknownCycle: nothing is scheduled or charged
//
// The staleness window (last offset + slack) fits inside one cycle, so a
// subscription never still duns the old period when the next charge is due.
const (
	// MinRetryCycleHours is the shortest billing cycle with any retries; below
	// it the first failure is terminal.
	MinRetryCycleHours = 4 * 24
	// MonthlyCycleHours is where the monthly tier starts.
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
	UpsertReconciliationFinding(context.Context, gen.UpsertReconciliationFindingParams) (gen.BillingReconciliationFinding, error)
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

// RetryOffsets returns the default retry schedule for a billing cycle in hours.
// Empty means no retries. Callers must not mutate the returned slice.
func RetryOffsets(cycleHours int) ([]time.Duration, error) {
	return DefaultPolicy.RetryOffsets(cycleHours)
}

// MaxFailures returns how many consecutive failures (counting the initial
// one) a billing cycle tolerates before collection goes terminal.
func MaxFailures(cycleHours int) (int, error) {
	return DefaultPolicy.MaxFailures(cycleHours)
}

// NextRetryIn returns how long after the failures-th consecutive failure
// (1-based) the next retry runs, or 0 when that failure is terminal. It is
// relative to the failure just made, so a late worker never schedules into the
// past.
func NextRetryIn(cycleHours, failures int) (time.Duration, error) {
	return DefaultPolicy.NextRetryIn(cycleHours, failures)
}

// NextAttemptAt is the schedule's next attempt after the failures-th
// consecutive failure (1-based) made at lastAttempt. ok is false when that
// failure spent the schedule. Every consumer takes retry times from here.
func NextAttemptAt(cycleHours, failures int, lastAttempt time.Time) (next time.Time, ok bool, err error) {
	return DefaultPolicy.NextAttemptAt(cycleHours, failures, lastAttempt)
}

// Window returns the derived staleness window: how long past the missed charge
// collection may still attempt it. Past it the charge is skipped (no surprise
// catch-up charge) and the row parks for provider verification; expiry alone is
// never terminal.
//
// window = last retry offset + min(24h, cycle/2), so a 0-retry cycle still gets
// the slack and Window(cycle) < cycle for every known cycle.
func Window(cycleHours int) (time.Duration, error) {
	return DefaultPolicy.Window(cycleHours)
}

func windowSlack(cycle time.Duration) time.Duration {
	return min(maxWindowSlack, cycle/2)
}

// BillingCycleHoursOf returns the price's billing cycle in hours, or 0 when
// unknown (one-time prices), which the schedule refuses with ErrUnknownCycle.
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

// CycleHoursBetween returns a billing period's length in hours: an invoice's
// cycle is its statement period. A degenerate or unset period returns 0
// (ErrUnknownCycle).
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
