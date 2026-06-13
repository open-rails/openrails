//go:build integration

package ratelimit_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/stretchr/testify/require"
)

// TestAcquireUnits_WeightedPool: the weighted reservation primitive holds N
// units up to max, denies the overflow without consuming, and releases by
// amount (#472 G2a).
func TestAcquireUnits_WeightedPool(t *testing.T) {
	l, ctx := newLimiter(t)
	key := uuid.NewString()
	const max = int64(1000)
	ttl := time.Minute

	ok, cur, err := l.AcquireUnits(ctx, key, 600, max, ttl)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(600), cur)

	ok, cur, err = l.AcquireUnits(ctx, key, 300, max, ttl)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(900), cur)

	// 900 + 200 = 1100 > 1000 -> denied, nothing consumed.
	ok, cur, err = l.AcquireUnits(ctx, key, 200, max, ttl)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, int64(900), cur, "denied acquire must not consume units")

	// Release 600 -> 300 in flight; the 200 now fits.
	require.NoError(t, l.ReleaseUnits(ctx, key, 600))
	n, err := l.UnitsCount(ctx, key)
	require.NoError(t, err)
	require.Equal(t, int64(300), n)

	ok, cur, err = l.AcquireUnits(ctx, key, 200, max, ttl)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(500), cur)
}

// TestReleaseUnits_FloorsAtZero: a release beyond the held sum floors at 0
// (double-release / TTL-expiry safety).
func TestReleaseUnits_FloorsAtZero(t *testing.T) {
	l, ctx := newLimiter(t)
	key := uuid.NewString()
	_, _, err := l.AcquireUnits(ctx, key, 100, 1000, time.Minute)
	require.NoError(t, err)
	require.NoError(t, l.ReleaseUnits(ctx, key, 500))
	n, err := l.UnitsCount(ctx, key)
	require.NoError(t, err)
	require.Equal(t, int64(0), n)
}

// TestAcquireQueue_HoldAndReleaseByRequest: the batch-queue reservation holds a
// request's units across pools, denies on overflow with the blocking unit, and
// frees by request coords at settlement (#472 G2b/G2c).
func TestAcquireQueue_HoldAndReleaseByRequest(t *testing.T) {
	l, ctx := newLimiter(t)
	base := uuid.NewString() // stands in for tenant:payer:release
	limits := []ratelimit.QueueLimit{{Unit: "request", Max: 2}}
	ttl := time.Minute

	// Two requests in queue: both held.
	d, err := l.AcquireQueue(ctx, base, "batch", "j1", limits, map[string]int64{"request": 1}, ttl)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	d, err = l.AcquireQueue(ctx, base, "batch", "j2", limits, map[string]int64{"request": 1}, ttl)
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Third would exceed the pool of 2 -> denied with the blocking unit.
	d, err = l.AcquireQueue(ctx, base, "batch", "j3", limits, map[string]int64{"request": 1}, ttl)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "request", d.BlockedUnit)

	// Complete j1 -> frees its slot; j3 now fits.
	require.NoError(t, l.ReleaseQueueByRequest(ctx, "batch", "j1"))
	d, err = l.AcquireQueue(ctx, base, "batch", "j3", limits, map[string]int64{"request": 1}, ttl)
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// Release is idempotent: a second release of j1 is a no-op.
	require.NoError(t, l.ReleaseQueueByRequest(ctx, "batch", "j1"))

	// Replay of an already-queued job is allowed without double-acquiring.
	d, err = l.AcquireQueue(ctx, base, "batch", "j2", limits, map[string]int64{"request": 1}, ttl)
	require.NoError(t, err)
	require.True(t, d.Allowed)
}

// TestAcquireQueue_WeightedTokens: the queue pool is weighted (token-denominated)
// — a request holds its token weight, and the pool caps the in-flight token sum.
func TestAcquireQueue_WeightedTokens(t *testing.T) {
	l, ctx := newLimiter(t)
	base := uuid.NewString()
	limits := []ratelimit.QueueLimit{{Unit: "token", Max: 1000}}
	ttl := time.Minute

	d, err := l.AcquireQueue(ctx, base, "batch", "t1", limits, map[string]int64{"token": 800}, ttl)
	require.NoError(t, err)
	require.True(t, d.Allowed)
	// 800 + 300 = 1100 > 1000 -> denied.
	d, err = l.AcquireQueue(ctx, base, "batch", "t2", limits, map[string]int64{"token": 300}, ttl)
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "token", d.BlockedUnit)
	// Free t1 -> 0 in flight; t2 fits.
	require.NoError(t, l.ReleaseQueueByRequest(ctx, "batch", "t1"))
	d, err = l.AcquireQueue(ctx, base, "batch", "t2", limits, map[string]int64{"token": 300}, ttl)
	require.NoError(t, err)
	require.True(t, d.Allowed)
}
