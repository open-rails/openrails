// Package delinquency owns the arrears delinquency state machine and the
// signal that goes with it (or#878).
//
// TWO AXES, and conflating them is the mistake this package exists to prevent:
//
//   - The DECLINE BUCKET (or#870, internal/modules/collection) answers *why a
//     charge failed* ⇒ what to do about the CARD. Time-independent. An expired
//     card is bucket 2 whether it expired today or in March.
//   - DELINQUENCY answers *how long a debt has gone unpaid* ⇒ what to do about
//     SERVICE. Amount- and time-based, and independent of why any single charge
//     failed. A payer with no card on file at all can be delinquent; a payer
//     whose card just declined is not, until the clock says so.
//
// Neither implies the other, and neither may be inferred from the other.
//
// THE BOUNDARY. OpenRails does not run the customer's VMs, seats or storage,
// and must never pretend to. What it owns is:
//
//  1. the STATE — current → grace → delinquent, per (merchant, payer,
//     currency), derived from the payer's overdue open receivables against the
//     merchant's declared grace window and amount floor;
//  2. one ENFORCEMENT lever it is genuinely authoritative over — admission
//     refusal, which for usage billing is the meaningful cutoff because it stops
//     the bill growing;
//  3. the SIGNAL — a durable, acknowledged host-lifecycle event on every
//     transition, in and out.
//
// What it must NOT do: revoke entitlements. The service was already consumed;
// retracting access does not recover the loss, and doing it on our own judgement
// is exactly the class of failure that costs a paying customer their access.
// The host shuts things off, because only the host knows what it is running.
package delinquency

import (
	"fmt"
	"strings"
	"time"
)

// State is the delinquency level for one (merchant, payer, currency).
type State string

const (
	// StateCurrent — nothing overdue. The absence of a debt, not a judgement.
	StateCurrent State = "current"

	// StateGrace — the payer owes money past its due date, but either the grace
	// window has not elapsed or the debt is below the merchant's floor. Visible
	// to the operator, enforced against by nothing.
	StateGrace State = "grace"

	// StateDelinquent — the debt has outlived grace and is large enough to
	// matter. New spend is refused; the host is told, and decides the rest.
	StateDelinquent State = "delinquent"
)

func (s State) String() string { return string(s) }

// Valid reports whether s is one of the three declared states.
func (s State) Valid() bool {
	switch s {
	case StateCurrent, StateGrace, StateDelinquent:
		return true
	}
	return false
}

// ParseState maps a stored value onto a State, defaulting to current — an
// unreadable state must never be read as "cut this customer off".
func ParseState(v string) State {
	s := State(strings.ToLower(strings.TrimSpace(v)))
	if !s.Valid() {
		return StateCurrent
	}
	return s
}

// DefaultGraceDays is the grace window a merchant that has declared none gets.
//
// Two weeks, deliberately generous. The default has to be the safe answer,
// because the merchants who never touch this knob are exactly the ones who have
// not thought about it — and the cost of being late to call someone delinquent
// is a few more days of accrual, while the cost of being early is cutting off a
// customer who was going to pay.
const DefaultGraceDays = 14

// DefaultAmountFloor is the smallest overdue balance that can make a payer
// delinquent when the merchant has declared neither a delinquency floor nor an
// invoice monthly floor: one whole currency unit, in micros.
//
// Kept equal to money.DefaultInvoiceMonthlyFloorAmount (pinned by a test rather
// than an import — the derivation below is the point: a debt the merchant has
// already declared too small to bother COLLECTING is too small to cut anyone
// off for).
const DefaultAmountFloor int64 = 1_000_000

// Policy is the merchant's declared delinquency policy. Two knobs, both with a
// default, and the second one derived rather than asked for wherever the
// merchant has already answered the same question.
type Policy struct {
	// GraceDays is how long past an invoice's due_at the payer keeps grace.
	// Zero is a valid explicit choice: delinquent the moment it is overdue.
	GraceDays int
	// AmountFloor (micros) is the smallest overdue balance that can escalate to
	// delinquent. Below it the payer sits in grace indefinitely — visible, but
	// never a reason to refuse anyone's work.
	AmountFloor int64
}

// Grace is the policy's grace window as a duration.
func (p Policy) Grace() time.Duration { return time.Duration(p.GraceDays) * 24 * time.Hour }

// Validate rejects a policy that cannot be honoured. Negative values are a
// config error, not something to silently clamp.
func (p Policy) Validate() error {
	if p.GraceDays < 0 {
		return fmt.Errorf("delinquency: arrears_grace_days must be >= 0, got %d", p.GraceDays)
	}
	if p.AmountFloor < 0 {
		return fmt.Errorf("delinquency: arrears_delinquency_floor must be >= 0, got %d", p.AmountFloor)
	}
	return nil
}

// Exposure is the invoice-derived input to a delinquency decision: how much a
// payer owes past its due date, since when, and across how many invoices.
type Exposure struct {
	OverdueSince    time.Time
	OverdueAmount   int64
	OverdueInvoices int
}

// Owes reports whether there is any overdue debt at all.
func (e Exposure) Owes() bool { return e.OverdueInvoices > 0 && e.OverdueAmount > 0 }

// Classify is THE decision, and it is a pure function of invoice truth and the
// merchant's policy. Everything stored about delinquency is recomputable
// through here; the stored row exists only to remember when a state started and
// whether its transition has already been announced.
//
// Ordering matters: a debt below the floor is grace no matter how old, so a
// rounding remnant left on an old invoice can never cut anyone off.
func Classify(p Policy, e Exposure, now time.Time) State {
	if !e.Owes() {
		return StateCurrent
	}
	if e.OverdueAmount < p.AmountFloor {
		return StateGrace
	}
	if now.Before(e.OverdueSince.Add(p.Grace())) {
		return StateGrace
	}
	return StateDelinquent
}
