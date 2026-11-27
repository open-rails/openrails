package collection

import (
	"errors"
	"fmt"
	"time"
)

// Policy is a merchant's dunning schedule as data. Tiers are ordered by cycle;
// the first tier whose MaxCycle exceeds a billing cycle applies, and the last
// (MaxCycle zero) takes every longer cycle. Offsets are measured from the first
// decline. Transient is the quick ladder for processor try-again answers.
type Policy struct {
	Tiers     []Tier
	Transient []time.Duration
	// CustomerRetries is how many declined attempts of the customer's own one
	// renewal takes on one card; then only another card is charged.
	CustomerRetries int
	// SuspendAccess ends access with the paid period while a declined renewal
	// is retried. By default access lasts until a confirmed outcome.
	SuspendAccess bool
	// SuspendWhenHeld ends an engine member's access at the renewal allowance
	// when collection is stopped and nothing was sent. An NMI charge sent but
	// unresolved keeps access under either policy. By default
	// access lasts until the renewal is attempted.
	SuspendWhenHeld bool
}

// Tier is the schedule for billing cycles shorter than MaxCycle.
type Tier struct {
	MaxCycle time.Duration
	Offsets  []time.Duration
}

// The card networks' limits on charging a card again after a decline: Visa
// allows 15 reattempts of a declined transaction in 30 days; Mastercard's
// excessive-attempt limits are 10 attempts in 24 hours and 35 in 30 days.
// Visa's is the tighter 30-day limit.
const (
	networkReattempts30d = 15
	networkAttempts24h   = 10
)

const (
	// DefaultCustomerRetries applies when a policy names none.
	DefaultCustomerRetries = 3
	// MaxCustomerRetries is the most any policy may allow: what the 24-hour
	// limit leaves beside the renewal's first attempt.
	MaxCustomerRetries = networkAttempts24h - 1
)

// DefaultPolicy is the built-in schedule: no retries below 4 days, +1d/+2d
// below 28 days, +2d/+4d/+6d/+9d from 28 days.
var DefaultPolicy = Policy{
	Tiers: []Tier{
		{MaxCycle: MinRetryCycleHours * time.Hour},
		{MaxCycle: MonthlyCycleHours * time.Hour, Offsets: offsetsWeekly},
		{Offsets: offsetsMonthly},
	},
	Transient:       TransientLadder,
	CustomerRetries: DefaultCustomerRetries,
}

// Validate refuses a policy that could dun a period into the next one: every
// tier's window (last offset + slack) must end inside the shortest cycle it
// covers, offsets must increase, and the ladder stays short and bounded. It
// also refuses one whose attempts on a card for one renewal (the first, every
// scheduled and transient retry, and the customer's own) could pass the card
// networks' limits.
func (p Policy) Validate() error {
	if len(p.Tiers) == 0 {
		return errors.New("dunning policy needs at least one tier")
	}
	lower := time.Hour
	for i, t := range p.Tiers {
		last := i == len(p.Tiers)-1
		if last != (t.MaxCycle == 0) {
			return errors.New("only the last dunning tier is open-ended")
		}
		if !last && t.MaxCycle <= lower {
			return fmt.Errorf("dunning tier %d: cycles must increase", i+1)
		}
		if len(t.Offsets) > 10 {
			return fmt.Errorf("dunning tier %d: at most 10 retries", i+1)
		}
		prev := time.Duration(0)
		for _, o := range t.Offsets {
			if o <= prev {
				return fmt.Errorf("dunning tier %d: retry offsets must be positive and increasing", i+1)
			}
			prev = o
		}
		if prev >= lower-windowSlack(lower) {
			return fmt.Errorf("dunning tier %d: retries must end well inside the shortest cycle it covers (%s)", i+1, lower)
		}
		if !last {
			lower = t.MaxCycle
		}
	}
	if len(p.Transient) > 3 {
		return errors.New("at most 3 transient retries")
	}
	for _, d := range p.Transient {
		if d < time.Minute || d > 2*time.Hour {
			return errors.New("transient retries run between 1 minute and 2 hours after the attempt")
		}
	}
	if p.CustomerRetries < 0 || p.CustomerRetries > MaxCustomerRetries {
		return fmt.Errorf("customer_retries must be between 0 and %d", MaxCustomerRetries)
	}
	// Transient and customer retries can land at any time, so they count in
	// every window; a case's scheduled retries all count toward the 30 days.
	anytime := len(p.Transient) + p.CustomerRetries
	for i, t := range p.Tiers {
		if n := len(t.Offsets) + anytime; n > networkReattempts30d {
			return fmt.Errorf("dunning tier %d: %d retries of one renewal on a card (scheduled, transient and the customer's) pass the card networks' %d in 30 days", i+1, n, networkReattempts30d)
		}
		if n := busiestDay(t.Offsets) + anytime; n > networkAttempts24h {
			return fmt.Errorf("dunning tier %d: %d attempts of one renewal on a card within 24 hours (scheduled, transient and the customer's) pass the card networks' %d", i+1, n, networkAttempts24h)
		}
	}
	return nil
}

// busiestDay is the most scheduled attempts, the first failure included, that
// fall within any 24 hours.
func busiestDay(offsets []time.Duration) int {
	at := append([]time.Duration{0}, offsets...)
	most := 0
	for i, from := range at {
		n := 0
		for _, o := range at[i:] {
			if o-from <= 24*time.Hour {
				n++
			}
		}
		most = max(most, n)
	}
	return most
}

// RetryOffsets is the schedule for a billing cycle in hours.
func (p Policy) RetryOffsets(cycleHours int) ([]time.Duration, error) {
	if cycleHours <= 0 {
		return nil, &UnknownCycleError{CycleHours: cycleHours}
	}
	cycle := time.Duration(cycleHours) * time.Hour
	for _, t := range p.Tiers {
		if t.MaxCycle == 0 || cycle < t.MaxCycle {
			return t.Offsets, nil
		}
	}
	return nil, nil
}

// MaxFailures is how many consecutive failures, the first included, a cycle
// tolerates before collection goes terminal.
func (p Policy) MaxFailures(cycleHours int) (int, error) {
	offsets, err := p.RetryOffsets(cycleHours)
	return len(offsets) + 1, err
}

// NextRetryIn is the gap after the failures-th failure (1-based), or 0 when
// that failure spends the schedule.
func (p Policy) NextRetryIn(cycleHours, failures int) (time.Duration, error) {
	offsets, err := p.RetryOffsets(cycleHours)
	if err != nil || failures < 1 || failures > len(offsets) {
		return 0, err
	}
	if failures == 1 {
		return offsets[0], nil
	}
	return offsets[failures-1] - offsets[failures-2], nil
}

// NextAttemptAt is the next attempt after the failures-th failure at
// lastAttempt; ok is false when the schedule is spent.
func (p Policy) NextAttemptAt(cycleHours, failures int, lastAttempt time.Time) (time.Time, bool, error) {
	gap, err := p.NextRetryIn(cycleHours, failures)
	if err != nil || gap == 0 {
		return time.Time{}, false, err
	}
	return lastAttempt.Add(gap), true, nil
}

// Outlook is what remains of a case with failures counted declines whose
// next attempt runs at next: the attempts still to come if each declines, and
// when the last of them runs. Each later attempt chains on the one before, as
// NextAttemptAt schedules it.
func (p Policy) Outlook(cycleHours, failures int, next time.Time) (left int, final time.Time, err error) {
	most, err := p.MaxFailures(cycleHours)
	if err != nil {
		return 0, time.Time{}, err
	}
	left = max(most-max(failures, 0), 0)
	final = next
	for f := max(failures, 0) + 1; f < most; f++ {
		at, ok, err := p.NextAttemptAt(cycleHours, f, final)
		if err != nil || !ok {
			return left, final, err
		}
		final = at
	}
	return left, final, nil
}

// CaseEnd is when collection of the renewal due at due stops: the window's
// end, or later, the slack past the last retry the schedule will make from
// next (Outlook) when the networks' advice delayed a retry, though that never
// runs past renewal, the next renewal's due time. failures counts the case's
// declines so far; next is its scheduled retry, nil for none.
func (p Policy) CaseEnd(cycleHours int, due, renewal time.Time, failures int, next *time.Time) (time.Time, error) {
	window, err := p.Window(cycleHours)
	if err != nil {
		return time.Time{}, err
	}
	end := due.Add(window)
	if next == nil {
		return end, nil
	}
	_, final, err := p.Outlook(cycleHours, failures, *next)
	if err != nil {
		return time.Time{}, err
	}
	last := final.Add(windowSlack(time.Duration(cycleHours) * time.Hour))
	if last.After(renewal) {
		last = renewal
	}
	if last.After(end) {
		end = last
	}
	return end, nil
}

// LongestWindow is the longest window any cycle gets.
func (p Policy) LongestWindow() time.Duration {
	last := time.Duration(0)
	for _, t := range p.Tiers {
		if n := len(t.Offsets); n > 0 {
			last = max(last, t.Offsets[n-1])
		}
	}
	return last + maxWindowSlack
}

// Window is how long past the missed charge collection may still attempt it.
func (p Policy) Window(cycleHours int) (time.Duration, error) {
	offsets, err := p.RetryOffsets(cycleHours)
	if err != nil {
		return 0, err
	}
	slack := windowSlack(time.Duration(cycleHours) * time.Hour)
	if len(offsets) == 0 {
		return slack, nil
	}
	return offsets[len(offsets)-1] + slack, nil
}

// NextTransientAttempt is when the used+1-th transient retry runs, or false
// when the ladder is spent.
func (p Policy) NextTransientAttempt(used int, at time.Time) (time.Time, bool) {
	if used < 0 || used >= len(p.Transient) {
		return time.Time{}, false
	}
	return at.Add(p.Transient[used]), true
}
