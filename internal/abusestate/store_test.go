package abusestate

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// Without Redis: counts per clock window, and holds until released or
// expired.
func TestMemory(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 30, 0, time.UTC)
	s := New(nil)
	s.SetClock(func() time.Time { return now })

	for i := int64(1); i <= 3; i++ {
		count, reset := s.Count(ctx, "rl:a", 1, time.Minute)
		require.Equal(t, i, count)
		require.Equal(t, 30*time.Second, reset)
	}
	require.NoError(t, s.ResetCounts(ctx, time.Minute, "rl:a"))
	count, _ := s.Count(ctx, "rl:a", 2, time.Minute)
	require.EqualValues(t, 2, count)
	now = now.Add(30 * time.Second)
	count, _ = s.Count(ctx, "rl:a", 1, time.Minute)
	require.EqualValues(t, 1, count, "the next clock minute starts over")

	s.Hold(ctx, "lock", time.Hour)
	require.Equal(t, time.Hour, s.Held(ctx, "lock"))
	require.NoError(t, s.Release(ctx, "lock"))
	require.Zero(t, s.Held(ctx, "lock"))
	s.Hold(ctx, "lock", time.Minute)
	now = now.Add(time.Minute)
	require.Zero(t, s.Held(ctx, "lock"), "expired")

	require.Zero(t, s.Fallbacks())
}

// Memory is bounded: expired entries go first, then the entry closest to
// expiry, and neither costs a pass over every key.
func TestMemoryIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	tb := newTable(3)
	tb.set("expired", now.Add(time.Second), now)
	tb.set("soon", now.Add(time.Hour), now)
	tb.set("late", now.Add(3*time.Hour), now)
	now = now.Add(2 * time.Second)
	tb.set("new", now.Add(2*time.Hour), now)
	require.Equal(t, 3, tb.len())
	require.Zero(t, tb.left("expired", now))

	tb.set("newer", now.Add(2*time.Hour), now)
	require.Equal(t, 3, tb.len())
	require.Zero(t, tb.left("soon", now), "the entry closest to expiry is dropped")
	require.Positive(t, tb.left("late", now))

	require.EqualValues(t, 2, tb.add("late", 1, now.Add(time.Minute), now), "a live entry keeps counting")
	require.Equal(t, now.Add(3*time.Hour-2*time.Second).Sub(now), tb.left("late", now), "and keeps its expiry")

	big := newTable(1000)
	for i := range 5000 {
		big.add(strconv.Itoa(i), 1, now.Add(time.Duration(i)*time.Second), now)
	}
	require.Equal(t, 1000, big.len())
	require.Zero(t, big.left("3999", now))
	require.Positive(t, big.left("4000", now))
	now = now.Add(4500 * time.Second)
	big.sweep(now)
	require.Equal(t, 499, big.len())
}

// A configured Redis that does not answer is left for memory: every
// operation still answers, and the store reports why and how often.
func TestUnreachableRedisFallsBackToMemory(t *testing.T) {
	ctx := context.Background()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: time.Second})
	t.Cleanup(func() { _ = rdb.Close() })
	s := New(rdb)
	require.ErrorIs(t, s.RedisErr(), errNotReached)

	count, _ := s.Count(ctx, "rl:a", 1, time.Minute)
	require.EqualValues(t, 1, count)
	require.Error(t, s.RedisErr())
	count, _ = s.Count(ctx, "rl:a", 1, time.Minute)
	require.EqualValues(t, 2, count)
	s.Hold(ctx, "lock", time.Minute)
	require.Positive(t, s.Held(ctx, "lock"))
	require.Error(t, s.Release(ctx, "lock"), "Redis was not told")
	require.Zero(t, s.Held(ctx, "lock"), "memory was")
	require.EqualValues(t, 5, s.Fallbacks(), "each operation memory took")
	require.Error(t, s.Probe(ctx))
}
