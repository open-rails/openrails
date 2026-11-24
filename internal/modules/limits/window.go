// Package limits is the usage-limit model: declared limits, the capacity
// products and overrides give a customer, and its windows, which the server
// clock identifies.
package limits

import (
	"fmt"
	"math/bits"
	"time"

	"github.com/ccoveille/go-safecast/v2"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/eventstore"
)

// Epoch is the window start of a lifetime or rate limit's one row.
var Epoch = time.Unix(0, 0).UTC()

// RollingBuckets is how many buckets a rolling window is counted in.
const RollingBuckets = 60

// Window is a limit's window as the server clock places it. A session or
// first-use window not yet opened is not Open; a lifetime or rate limit's
// one row starts at Epoch and has no end.
type Window struct {
	StartsAt time.Time
	EndsAt   *time.Time
	Open     bool
}

// Stored is one limit window row.
type Stored struct {
	StartsAt  time.Time
	EndsAt    *time.Time
	Adjusted  int64
	ReachedAt *time.Time
	UpdatedAt time.Time
}

// Anchors place a customer's windows: the start of its subscription to a
// product giving the limit (nil: none), and its pricing period at an instant.
type Anchors struct {
	Subscription *time.Time
	Period       func(at time.Time) (start, end time.Time, err error)
}

// Locate is the window now falls in. latest is the limit's newest stored
// window, nil when it has none: a first-use window tiles from it and a
// session is open until it ends. A rolling limit's window is the bucket now
// falls in.
func Locate(reset catalog.LimitReset, latest *Stored, anchors Anchors, now time.Time) (Window, error) {
	now = now.UTC()
	width := time.Duration(reset.WindowSeconds) * time.Second
	switch reset.Kind {
	case catalog.ResetLifetime, catalog.ResetRate:
		return Window{StartsAt: Epoch, Open: true}, nil
	case catalog.ResetFixed:
		var anchor time.Time
		switch {
		case reset.Anchor == catalog.AnchorFirstUse && latest == nil:
			return Window{}, nil
		case reset.Anchor == catalog.AnchorFirstUse:
			anchor = latest.StartsAt
		case anchors.Subscription != nil:
			anchor = *anchors.Subscription
		default:
			anchor = Epoch
		}
		return span(tile(anchor, now, width), width), nil
	case catalog.ResetSession:
		if latest != nil && latest.EndsAt != nil && !now.Before(latest.StartsAt) && now.Before(*latest.EndsAt) {
			return Window{StartsAt: latest.StartsAt, EndsAt: latest.EndsAt, Open: true}, nil
		}
		return Window{}, nil
	case catalog.ResetRolling:
		bucket := BucketWidth(reset.WindowSeconds)
		return span(tile(Epoch, now, bucket), bucket), nil
	case catalog.ResetBillingPeriod:
		if anchors.Period == nil {
			return Window{}, fmt.Errorf("limit window: no pricing period")
		}
		start, end, err := anchors.Period(now)
		if err != nil {
			return Window{}, err
		}
		return Window{StartsAt: start.UTC(), EndsAt: ptr(end.UTC()), Open: true}, nil
	}
	return Window{}, fmt.Errorf("limit window: unknown reset %q", reset.Kind)
}

// Opening is the window a use at now opens when Locate found none open: a
// session or first-use window starting at that second.
func Opening(reset catalog.LimitReset, now time.Time) Window {
	start := now.UTC().Truncate(time.Second)
	return span(start, time.Duration(reset.WindowSeconds)*time.Second)
}

// BucketWidth is a rolling window's bucket: a RollingBuckets-th of it, at
// least a second.
func BucketWidth(windowSeconds int) time.Duration {
	return time.Duration(max(windowSeconds/RollingBuckets, 1)) * time.Second
}

// BucketAt is the bucket of a rolling or rate limit at falls in.
func BucketAt(windowSeconds int, at time.Time) Window {
	width := BucketWidth(windowSeconds)
	return span(tile(Epoch, at.UTC(), width), width)
}

// RollingFrom is the start of the oldest bucket a rolling window counts at
// now: the one the window's start falls in.
func RollingFrom(windowSeconds int, now time.Time) time.Time {
	return tile(Epoch, now.UTC().Add(-time.Duration(windowSeconds)*time.Second), BucketWidth(windowSeconds))
}

// RateBuckets is how many buckets a rate limit's level is read from: two
// windows' worth, past which a bucket has drained by twice the capacity.
const RateBuckets = 2 * RollingBuckets

// RateFrom is the start of the oldest bucket a rate limit's level reads at
// now.
func RateFrom(windowSeconds int, now time.Time) time.Time {
	bucket := BucketWidth(windowSeconds)
	return tile(Epoch, now.UTC(), bucket).Add(-RateBuckets * bucket)
}

// RateLevelOf is a rate limit's level at now from its buckets, in order: the
// most any run of the latest buckets holds beyond what drained, at capacity
// per window, since the run's first bucket ended. Counting a bucket's units
// at its end drains them no earlier than they arrived.
func RateLevelOf(buckets []eventstore.LimitRow, capacity int64, windowSeconds int, now time.Time) int64 {
	if len(buckets) == 0 {
		return 0
	}
	width := BucketWidth(windowSeconds)
	window := time.Duration(windowSeconds) * time.Second
	var level, run int64
	for i := len(buckets) - 1; i >= 0; i-- {
		run = addSat(run, buckets[i].Units)
		drained := int64(0)
		if elapsed := now.Sub(buckets[i].WindowStartsAt.Add(width)); elapsed > 0 && capacity > 0 {
			drained = signed(mulDiv(unsigned(capacity), unsigned(int64(elapsed)), unsigned(int64(window))))
		}
		level = max(level, subSat(run, drained))
	}
	return level
}

// RateLevel is a rate limit's level at now: the level it held at an
// instant, drained at capacity per window since.
func RateLevel(level, capacity int64, windowSeconds int, at, now time.Time) int64 {
	if level <= 0 || capacity <= 0 || !now.After(at) {
		return level
	}
	elapsed := now.Sub(at)
	window := time.Duration(windowSeconds) * time.Second
	if elapsed >= window {
		return max(level-capacity, 0)
	}
	drained := mulDiv(unsigned(capacity), unsigned(int64(elapsed)), unsigned(int64(window)))
	if drained >= unsigned(level) {
		return 0
	}
	return level - signed(drained)
}

// RateRefilledAt is when a rate limit at level drains to zero.
func RateRefilledAt(level, capacity int64, windowSeconds int, now time.Time) *time.Time {
	if level <= 0 || capacity <= 0 {
		return nil
	}
	window := unsigned(int64(windowSeconds)) * uint64(time.Second)
	seconds := (mulDiv(unsigned(level), window, unsigned(capacity)) + uint64(time.Second) - 1) / uint64(time.Second)
	return ptr(now.UTC().Truncate(time.Second).Add(time.Duration(signed(min(seconds, uint64(1)<<32))) * time.Second))
}

// unsigned is a nonnegative int64 as uint64, zero below it.
func unsigned(v int64) uint64 { return safecast.MustConvert[uint64](max(v, 0)) }

// signed is a uint64 as int64, saturating.
func signed(v uint64) int64 { return safecast.MustConvert[int64](min(v, uint64(maxInt64))) }

// tile is the start of the width-wide tile from anchor that now falls in.
func tile(anchor, now time.Time, width time.Duration) time.Time {
	elapsed := now.Sub(anchor)
	n := elapsed / width
	if elapsed < 0 && elapsed%width != 0 {
		n--
	}
	return anchor.Add(n * width).UTC()
}

func span(start time.Time, width time.Duration) Window {
	return Window{StartsAt: start, EndsAt: ptr(start.Add(width)), Open: true}
}

// mulDiv is a×b÷c in 128 bits, saturating at the largest uint64.
func mulDiv(a, b, c uint64) uint64 {
	hi, lo := bits.Mul64(a, b)
	if hi >= c {
		return ^uint64(0)
	}
	q, _ := bits.Div64(hi, lo, c)
	return q
}

func ptr[T any](v T) *T { return &v }
