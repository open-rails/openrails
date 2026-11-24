package limits

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/eventstore"
)

// A rate limit's level is read from its buckets: the most any run of the
// latest buckets holds beyond what drained since the run's first bucket
// ended, never below zero.
func TestRateLevelOfBuckets(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 30, 0, time.UTC)
	// A 60-second window has 1-second buckets; capacity 60 drains one unit a
	// second.
	bucket := func(secondsAgo int, units int64) eventstore.LimitRow {
		return eventstore.LimitRow{Key: "rate", WindowStartsAt: now.Truncate(time.Second).Add(-time.Duration(secondsAgo) * time.Second), Units: units}
	}
	require.Zero(t, RateLevelOf(nil, 60, 60, now))

	// 30 units in the current bucket have not drained at all: counted at the
	// bucket's end, which is still ahead.
	require.EqualValues(t, 30, RateLevelOf([]eventstore.LimitRow{bucket(0, 30)}, 60, 60, now))

	// 30 units that ended 10 s ago drained 10.
	require.EqualValues(t, 20, RateLevelOf([]eventstore.LimitRow{bucket(11, 30)}, 60, 60, now))

	// An old burst that drained away leaves only the recent run.
	old := bucket(100, 50)
	recent := bucket(0, 5)
	require.EqualValues(t, 5, RateLevelOf([]eventstore.LimitRow{old, recent}, 60, 60, now))

	// The level is the most any suffix holds: a burst 5 s ago outweighs the
	// whole run's drain.
	burst := []eventstore.LimitRow{bucket(40, 10), bucket(6, 40), bucket(0, 2)}
	require.EqualValues(t, 40+2-5, RateLevelOf(burst, 60, 60, now))

	// It never reads below zero, and bucket width follows the window.
	require.Zero(t, RateLevelOf([]eventstore.LimitRow{bucket(59, 1)}, 60, 60, now))
	require.Equal(t, 2*time.Second, BucketWidth(120))
	require.Equal(t, time.Second, BucketWidth(30))
	require.True(t, RateFrom(60, now).Equal(now.Truncate(time.Second).Add(-RateBuckets*time.Second)))
}
