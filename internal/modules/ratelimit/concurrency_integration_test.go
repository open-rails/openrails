//go:build integration

package ratelimit_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestConcurrency_AcquireUpToMax(t *testing.T) {
	l, ctx := newLimiter(t)
	key := uuid.NewString()
	const max = int64(3)
	ttl := time.Minute

	for i := int64(1); i <= max; i++ {
		acquired, cur, err := l.AcquireConcurrency(ctx, key, max, ttl)
		require.NoError(t, err)
		require.True(t, acquired, "acquire %d of %d should succeed", i, max)
		require.Equal(t, i, cur)
	}

	// The (max+1)th in-flight request is rejected; nothing is consumed.
	acquired, cur, err := l.AcquireConcurrency(ctx, key, max, ttl)
	require.NoError(t, err)
	require.False(t, acquired, "acquire over max=%d must be rejected", max)
	require.Equal(t, max, cur, "rejected acquire must not consume a slot")

	got, err := l.ConcurrencyCount(ctx, key)
	require.NoError(t, err)
	require.Equal(t, max, got)
}

func TestConcurrency_ReleaseFreesSlot(t *testing.T) {
	l, ctx := newLimiter(t)
	key := uuid.NewString()
	const max = int64(2)
	ttl := time.Minute

	// Fill to capacity.
	for i := int64(0); i < max; i++ {
		acquired, _, err := l.AcquireConcurrency(ctx, key, max, ttl)
		require.NoError(t, err)
		require.True(t, acquired)
	}
	// At capacity: next acquire fails.
	acquired, _, err := l.AcquireConcurrency(ctx, key, max, ttl)
	require.NoError(t, err)
	require.False(t, acquired)

	// Release one slot, then a subsequent acquire succeeds again.
	require.NoError(t, l.ReleaseConcurrency(ctx, key))
	got, err := l.ConcurrencyCount(ctx, key)
	require.NoError(t, err)
	require.Equal(t, max-1, got)

	acquired, cur, err := l.AcquireConcurrency(ctx, key, max, ttl)
	require.NoError(t, err)
	require.True(t, acquired, "acquire after release should succeed")
	require.Equal(t, max, cur)
}

func TestConcurrency_ReleaseNeverNegative(t *testing.T) {
	l, ctx := newLimiter(t)
	key := uuid.NewString()

	// Release with no holders, repeatedly: count must floor at 0, never negative.
	for i := 0; i < 5; i++ {
		require.NoError(t, l.ReleaseConcurrency(ctx, key))
		got, err := l.ConcurrencyCount(ctx, key)
		require.NoError(t, err)
		require.Equal(t, int64(0), got, "release must never drive count below 0")
	}

	// Acquire once, then over-release: count floors at 0.
	acquired, _, err := l.AcquireConcurrency(ctx, key, 1, time.Minute)
	require.NoError(t, err)
	require.True(t, acquired)

	require.NoError(t, l.ReleaseConcurrency(ctx, key)) // back to 0
	require.NoError(t, l.ReleaseConcurrency(ctx, key)) // extra release
	got, err := l.ConcurrencyCount(ctx, key)
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}
