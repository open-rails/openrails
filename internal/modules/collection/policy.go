package collection

import (
	"errors"
	"fmt"
	"time"
)

// Policy is a merchant's dunning schedule as data (#1093). Tiers are ordered
// by cycle; the first tier whose MaxCycle exceeds a billing cycle applies, and
// the last tier (MaxCycle zero) takes every longer cycle. Retry offsets are
// measured from the first decline. Transient is the quick ladder for processor
// try-again answers.
type Policy struct {
	Tiers     []Tier
	Transient []time.Duration
	// SuspendAccess ends access with the paid period while a declined renewal
	// is retried. The default keeps access until a confirmed outcome (Paul,
	// 2026-09-25); uncertainty alone never ends it.
	SuspendAccess bool
	// SuspendWhenHeld ends an engine member's access at the renewal allowance
	// when collection is stopped and the renewal has no outcome. The default
	// keeps it until the renewal is attempted (Paul, 2026-09-25).
	SuspendWhenHeld bool
}

// Tier is the schedule for billing cycles shorter than MaxCycle.
type Tier struct {
	MaxCycle time.Duration
	Offsets  []time.Duration
}

// DefaultPolicy is the built-in schedule (#359): no retries below 4 days,
// +1d/+2d below 28 days, +2d/+5d/+9d/+13d from 28 days.
var DefaultPolicy = Policy{
	Tiers: []Tier{
		{MaxCycle: MinRetryCycleHours * time.Hour},
		{MaxCycle: MonthlyCycleHours * time.Hour, Offsets: offsetsWeekly},
		{Offsets: offsetsMonthly},
	},
	Transient: TransientLadder,
}

// Validate refuses a policy that could dun a period into the next one: every
// tier's window (last offset + slack) must end inside the shortest cycle it
// covers, offsets must increase, and the ladder stays short and bounded.
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
		if prev+windowSlack(lower) >= lower {
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
	return nil
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
