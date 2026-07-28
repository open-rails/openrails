package reconcile

import (
	"fmt"
	"math"
)

// #837/#834 — the pass-level brakes on convergence-driven cancellation.
//
// Every guard here bounds the LOCAL cancel + entitlement revoke, not just the
// provider intent. Gating only the provider intent (the #679 volume breaker)
// leaves the worst case uncovered: an `absent_from_exhaustive_roster` cancel
// carries RemoteGone=true, creates NO provider intent, and so is invisible to
// that breaker while it still revokes the customer's access.

const (
	// DefaultMaxCancelsPerPass is the absolute per-merchant, per-pass ceiling
	// on cancellations, mirroring DestructiveBudgetFloor. Converging a real
	// book never needs more than a handful per pass; a number in the hundreds
	// is a broken roster, not a churn event.
	DefaultMaxCancelsPerPass = 25
	// DefaultMaxCancelFraction is the share of a merchant's live book one pass
	// may cancel.
	DefaultMaxCancelFraction = 0.05
	// DefaultBreakerRatio: a roster reporting less than this share of the local
	// live book is more likely truncated/broken than true.
	DefaultBreakerRatio = 0.10
	// DefaultBreakerMinLocal is 1, NOT 10: a merchant with nine subscribers can
	// lose all nine, so small books need MORE protection, not an exemption.
	DefaultBreakerMinLocal = 1
)

// CancelBudget bounds how many local subscription cancellations one
// convergence pass may apply for one merchant. It is all-or-nothing by design:
// when a pass plans more cancellations than the budget allows, NONE of them are
// applied. A pass that wants to cancel 850 customers is not a pass that should
// cancel the first 25 of them.
type CancelBudget struct {
	// Max is the absolute ceiling (<=0 → DefaultMaxCancelsPerPass).
	Max int
	// Fraction is the share of the live book (<=0 → DefaultMaxCancelFraction).
	Fraction float64
}

// Limit is how many cancellations this pass may apply against a book of
// localLive live subscriptions: the tighter of the absolute and percentage
// caps, floored at 1.
//
// The floor of 1 is deliberate. Without it, P% of a nine-subscriber book
// rounds to zero and the merchant's mirror could never converge even ONE
// genuinely provider-cancelled subscription — entitlements would be granted
// forever off a subscription that no longer exists. One at a time still makes
// a mass cancellation structurally impossible.
func (b CancelBudget) Limit(localLive int) int {
	maxAbs := b.Max
	if maxAbs <= 0 {
		maxAbs = DefaultMaxCancelsPerPass
	}
	frac := b.Fraction
	if frac <= 0 {
		frac = DefaultMaxCancelFraction
	}
	limit := int(math.Floor(float64(localLive) * frac))
	if limit > maxAbs {
		limit = maxAbs
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// Exceeded reports whether a pass planning `planned` cancellations against a
// book of `localLive` live subscriptions blows the budget, with the operator-
// facing explanation.
func (b CancelBudget) Exceeded(planned, localLive int) (bool, string) {
	limit := b.Limit(localLive)
	if planned <= limit {
		return false, ""
	}
	return true, fmt.Sprintf(
		"cancellation cap: this pass planned %d cancellations against %d locally-live subscriptions, over the %d-row limit (max %d rows or %.0f%% of the book, whichever is smaller). NONE were applied and the merchant's pass halted — converging %d cancellations at once is a broken roster, not %d customers all leaving. Review the findings, then re-run once the provider data is trusted",
		planned, localLive, limit, b.maxAbs(), b.fraction()*100, planned, planned)
}

func (b CancelBudget) maxAbs() int {
	if b.Max <= 0 {
		return DefaultMaxCancelsPerPass
	}
	return b.Max
}

func (b CancelBudget) fraction() float64 {
	if b.Fraction <= 0 {
		return DefaultMaxCancelFraction
	}
	return b.Fraction
}

// RosterBreaker is the ratio guard on absence-as-truth: when a provider's
// exhaustive roster reports implausibly few live subscriptions against local
// state, refuse to treat absence as cancellation at all.
type RosterBreaker struct {
	// MinLocal is the smallest local live book the breaker applies to
	// (<=0 → DefaultBreakerMinLocal = 1: no small-merchant blind spot).
	MinLocal int
	// Ratio (<=0 → DefaultBreakerRatio).
	Ratio float64
}

func (r RosterBreaker) minLocal() int {
	if r.MinLocal > 0 {
		return r.MinLocal
	}
	return DefaultBreakerMinLocal
}

func (r RosterBreaker) ratio() float64 {
	if r.Ratio > 0 {
		return r.Ratio
	}
	return DefaultBreakerRatio
}

// Implausible reports whether the roster is too small to be believed, with the
// operator-facing explanation.
func (r RosterBreaker) Implausible(provider Provider, remoteLive, localLive int) (bool, string) {
	if localLive < r.minLocal() {
		return false, ""
	}
	if float64(remoteLive) >= float64(localLive)*r.ratio() {
		return false, ""
	}
	return true, fmt.Sprintf(
		"circuit breaker: %s reports only %d live subscriptions against %d locally-live linked subscriptions (< %.0f%%); refusing to treat absence as cancellation — the report is more likely truncated/broken than %d users all cancelled. No findings were generated; investigate the provider report before re-running",
		provider, remoteLive, localLive, r.ratio()*100, localLive)
}

// countPlannedCancellations counts the decider transitions in a diff that would
// terminally cancel a local subscription (and revoke its entitlements).
func countPlannedCancellations(findings []Finding) int {
	n := 0
	for i := range findings {
		a := findings[i].Apply
		if a != nil && a.Decide != nil && a.Decide.Decision.Kind == TransitionCancel {
			n++
		}
	}
	return n
}

// cancellationCapFinding is the operator-facing record of a halted pass. One
// stable subject per provider, so re-runs update rather than pile up.
func cancellationCapFinding(provider Provider, planned, localLive, remoteLive int, reason string) Finding {
	return Finding{
		Provider:      provider,
		Type:          FindingCancellationCapped,
		SubjectKey:    "cancellation_cap",
		Severity:      SeverityCritical,
		Status:        FindingStatusRequiresReview,
		RequiresAdmin: true,
		LocalEvidence: map[string]any{
			"planned_cancellations": planned,
			"local_live":            localLive,
		},
		RemoteEvidence: map[string]any{
			"remote_live": remoteLive,
		},
		RecommendedAction: reason,
	}
}
