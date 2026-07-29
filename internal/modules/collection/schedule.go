// Package collection is the ONE collection engine core (#828): "a user owes
// money; collect it on a schedule; classify outcomes; verify ambiguity; go
// terminal deliberately." Two consumers ride it — subscription rebill dunning
// (terminal ⇒ cancel + entitlement revoke, staleness ⇒ cancel-never-charge)
// and invoice arrears collection (terminal ⇒ uncollectible). Consumer POLICY
// stays at the consumer; the schedule table, outcome classification and
// failure-action mechanics live here, once.
package collection

import (
	"time"

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
//	unknown (<= 0)        -> monthly schedule, defensively (callers log)
//
// Boundary rationale: the derived staleness window (last offset + one day of
// slack) must fit WELL INSIDE one billing cycle, so a subscription is never
// still dunning the old period when the next charge is due.
const (
	// MinRetryCycleHours is the shortest billing cycle that gets any retries at
	// all; below it the first failure is terminal. 96h (4 days).
	MinRetryCycleHours = 4 * 24
	// MonthlyCycleHours is where the monthly (capped) tier starts — and the
	// cycle the invoice-arrears consumer bills on. 672h (28 days).
	MonthlyCycleHours = 28 * 24
	// windowSlack is added past the last retry offset when deriving the
	// staleness window: 24h tolerates a late worker run.
	windowSlack = 24 * time.Hour
)

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
// cycleHours <= 0 means unknown; the monthly schedule is returned defensively.
// Callers must not mutate the returned slice.
func RetryOffsets(cycleHours int) []time.Duration {
	switch {
	case cycleHours <= 0:
		return offsetsMonthly
	case cycleHours < MinRetryCycleHours:
		return nil
	case cycleHours < MonthlyCycleHours:
		return offsetsWeekly
	default:
		return offsetsMonthly
	}
}

// MaxFailures returns how many consecutive failures (counting the initial
// one) a billing cycle tolerates before collection goes terminal.
func MaxFailures(cycleHours int) int {
	return len(RetryOffsets(cycleHours)) + 1
}

// NextRetryIn returns how long after the failures-th consecutive failure
// (1-based) the next retry should run, or 0 when that failure is terminal.
// The gaps reproduce the offset schedule when each retry runs on time and
// degrade gracefully when the worker is late — the next retry is always
// relative to the failure that just happened, never in the past.
func NextRetryIn(cycleHours, failures int) time.Duration {
	offsets := RetryOffsets(cycleHours)
	if failures < 1 || failures > len(offsets) {
		return 0
	}
	if failures == 1 {
		return offsets[0]
	}
	return offsets[failures-1] - offsets[failures-2]
}

// Window returns the DERIVED staleness window (#344, #359): how long past the
// missed charge collection may still attempt one. Past it the charge is SKIPPED
// — a card that failed months ago is never surprise-charged by a catch-up run —
// and the row parks for provider verification. Expiry is NOT a terminal
// outcome (#839): a clock reading is not evidence a subscription is dead.
//
// window = last retry offset + one day of slack. A 0-retry cycle (sub-4-day
// cadence) has no offsets, so its window is the slack alone — NOT zero (#839):
// a zero window is true by construction the moment a row lapses, which would
// close every daily subscription on its first collection touch without ever
// attempting the charge it is queued for.
func Window(cycleHours int) time.Duration {
	offsets := RetryOffsets(cycleHours)
	if len(offsets) == 0 {
		return windowSlack
	}
	return offsets[len(offsets)-1] + windowSlack
}

// BillingCycleHoursOf returns the price's billing cycle in HOURS, or 0 when
// the price or its cycle is unknown (one-time prices) — the defensive
// monthly-fallback input to the schedule functions.
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
