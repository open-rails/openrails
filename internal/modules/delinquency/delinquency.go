// Package delinquency owns the arrears delinquency state (current → grace →
// delinquent per merchant, payer and currency), admission refusal for
// delinquent payers, and a host-lifecycle event on every transition.
//
// Delinquency is how long a debt has gone unpaid, independent of why any
// charge failed (the decline bucket); neither is inferred from the other. It
// never revokes entitlements: the host shuts things off, because only the host
// knows what it is running.
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

	// StateGrace — overdue, but within the grace window or below the
	// merchant's floor. Visible, never enforced.
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

// DefaultGraceDays is the grace window of a merchant that declared none:
// generous, since calling someone delinquent early cuts off a customer who
// was going to pay.
const DefaultGraceDays = 14

// DefaultAmountFloor is the smallest overdue balance (micros) that can make a
// payer delinquent when the merchant declared neither a delinquency nor an
// invoice monthly floor. It equals money.DefaultInvoiceMonthlyFloorAmount
// (pinned by a test): too small to collect is too small to cut anyone off for.
const DefaultAmountFloor int64 = 1_000_000

// Policy is the merchant's declared delinquency policy.
type Policy struct {
	// GraceDays is how long past an invoice's due_at the payer keeps grace.
	// Zero is a valid explicit choice: delinquent the moment it is overdue.
	GraceDays int
	// AmountFloor (micros) is the smallest overdue balance that can escalate to
	// delinquent; below it the payer stays in grace.
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
	OverdueStartedAt time.Time
	OverdueAmount    int64
	OverdueInvoices  int
}

// Owes reports whether there is any overdue debt at all.
func (e Exposure) Owes() bool { return e.OverdueInvoices > 0 && e.OverdueAmount > 0 }

// Classify is the decision: a pure function of invoice truth and policy. The
// stored row only remembers when a state started and whether it was announced.
// A debt below the floor is grace however old, so a rounding remnant never
// cuts anyone off.
func Classify(p Policy, e Exposure, now time.Time) State {
	if !e.Owes() {
		return StateCurrent
	}
	if e.OverdueAmount < p.AmountFloor {
		return StateGrace
	}
	if now.Before(e.OverdueStartedAt.Add(p.Grace())) {
		return StateGrace
	}
	return StateDelinquent
}

// withOverrides applies a bound billing policy's per-payer overrides on top of
// the merchant-wide policy. -1 is the query's no-override sentinel; 0 is a
// real "delinquent as soon as overdue".
func (p Policy) withOverrides(graceDays int, amountFloor int64) (Policy, error) {
	if graceDays >= 0 {
		p.GraceDays = graceDays
	}
	if amountFloor >= 0 {
		p.AmountFloor = amountFloor
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}
