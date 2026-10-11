package money

import (
	"fmt"
	"math"
	"math/bits"
	"time"
)

// microsPerHour converts between an amount drawn at a rate per hour and the
// microseconds it takes.
const microsPerHour = 3_600_000_000

// balanceHorizon is the furthest ahead a run-out is told: a balance that
// lasts longer runs out there.
const balanceHorizon = 100 * 365 * 24 * time.Hour

// RunningChargeTotal is what a charge at ratePerHour has charged by at: the
// rate times the whole seconds since startedAt (never negative), over 3600,
// rounded half up. billing.running_charge_total is the same rule in SQL, and
// running_charges checks every posting against it.
func RunningChargeTotal(ratePerHour int64, startedAt, at time.Time) (int64, error) {
	if ratePerHour < 0 {
		return 0, fmt.Errorf("running charge: rate is negative")
	}
	var seconds int64
	if micros := at.UnixMicro() - startedAt.UnixMicro(); micros > 0 {
		seconds = micros / 1_000_000
	}
	hi, lo := bits.Mul64(uint64(ratePerHour), uint64(seconds)) // #nosec G115 -- both are nonnegative
	lo, carry := bits.Add64(lo, 1800, 0)
	hi += carry
	if hi >= 3600 {
		return 0, fmt.Errorf("running charge: amount overflows")
	}
	total, _ := bits.Div64(hi, lo, 3600)
	if total > math.MaxInt64 {
		return 0, fmt.Errorf("running charge: amount overflows")
	}
	return int64(total), nil
}

// creditLot is a spendable credit lot: what is left of it and when it lapses.
type creditLot struct {
	remaining int64
	endsAt    *time.Time
}

// satSub and satAdd keep a projection inside int64: amounts that large are
// not real, and saturating never moves a run-out the wrong side of now.
func satSub(a, b int64) int64 {
	if b > 0 && a < math.MinInt64+b {
		return math.MinInt64
	}
	if b < 0 && a > math.MaxInt64+b {
		return math.MaxInt64
	}
	return a - b
}

func satAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// drawnBy is what ratePerHour draws in d, rounded down.
func drawnBy(ratePerHour int64, d time.Duration) int64 {
	if ratePerHour <= 0 || d <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(ratePerHour), uint64(d.Microseconds())) // #nosec G115 -- both are positive
	if hi >= microsPerHour {
		return math.MaxInt64
	}
	drawn, _ := bits.Div64(hi, lo, microsPerHour)
	if drawn > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(drawn)
}

// timeToDraw is how long ratePerHour takes to draw amount, rounded up to the
// microsecond and capped at balanceHorizon.
func timeToDraw(ratePerHour, amount int64) time.Duration {
	if amount <= 0 {
		return 0
	}
	hi, lo := bits.Mul64(uint64(amount), microsPerHour) // #nosec G115 -- positive
	rate := uint64(ratePerHour)                         // #nosec G115 -- the caller's rate is positive
	if hi >= rate {
		return balanceHorizon
	}
	micros, rem := bits.Div64(hi, lo, rate)
	if rem > 0 {
		micros++
	}
	if micros > uint64(balanceHorizon.Microseconds()) { // #nosec G115 -- a positive constant
		return balanceHorizon
	}
	return time.Duration(micros) * time.Microsecond // #nosec G115 -- bounded by balanceHorizon
}

// firstInstantAtOrUnder is the first instant a balance that stands at
// available at now is at or under floor, drawn at ratePerHour (positive) from
// its credit lots soonest-expiring first, each lot dropping what is left of it
// when it lapses: grants.CreditSpend's order. lots are the spendable lots at
// now in that order. available counts what is already claimed against them
// (holds, accrual), which is drawn first; money no lot holds is not counted.
func firstInstantAtOrUnder(now time.Time, available int64, lots []creditLot, ratePerHour, floor int64) time.Time {
	left := make([]creditLot, 0, len(lots))
	var spendable int64
	for _, lot := range lots {
		if lot.remaining > 0 {
			left = append(left, lot)
			spendable = satAdd(spendable, lot.remaining)
		}
	}
	if available > spendable {
		available = spendable
	}
	// What is claimed comes off the front lots; past them it is a deficit.
	claimed := satSub(spendable, available)
	for i := range left {
		take := min(left[i].remaining, claimed)
		left[i].remaining -= take
		claimed -= take
	}
	var drawn int64 // drawn at the rate since now
	for _, lot := range left {
		if available <= floor {
			break
		}
		if lot.remaining == 0 {
			continue
		}
		room := lot.remaining
		lapses := false
		if lot.endsAt != nil {
			byLapse := satSub(drawnBy(ratePerHour, lot.endsAt.Sub(now)), drawn)
			if byLapse < room {
				room, lapses = max(byLapse, 0), true
			}
		}
		if need := satSub(available, floor); need <= room {
			return now.Add(timeToDraw(ratePerHour, satAdd(drawn, need)))
		}
		drawn = satAdd(drawn, room)
		available = satSub(available, lot.remaining)
		if lapses && available <= floor {
			if lot.endsAt.Before(now) {
				return now
			}
			return *lot.endsAt
		}
	}
	if available <= floor {
		return now
	}
	return now.Add(timeToDraw(ratePerHour, satAdd(drawn, satSub(available, floor))))
}
