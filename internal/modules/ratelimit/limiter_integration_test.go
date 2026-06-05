//go:build integration

package ratelimit_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func newLimiter(t *testing.T) (*ratelimit.Limiter, context.Context) {
	t.Helper()
	ctx := context.Background()
	c, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Terminate(ctx) })
	conn, err := c.ConnectionString(ctx)
	require.NoError(t, err)
	opt, err := redis.ParseURL(conn)
	require.NoError(t, err)
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())
	return ratelimit.NewLimiter(rdb), ctx
}

func TestLimiter_RequestPerMinute(t *testing.T) {
	l, ctx := newLimiter(t)
	base := uuid.NewString()
	p := ratelimit.Policy{Windows: []ratelimit.Limit{{Unit: "request", Window: time.Minute, Max: 3}}}
	amt := map[string]int64{"request": 1}

	for i := 0; i < 3; i++ {
		d, err := l.Check(ctx, base, p, amt)
		require.NoError(t, err)
		require.True(t, d.Allowed, "request %d should pass", i+1)
	}
	d, err := l.Check(ctx, base, p, amt)
	require.NoError(t, err)
	require.False(t, d.Allowed, "4th request over RPM=3 must be blocked")
	require.Equal(t, "request", d.BlockedUnit)
	require.Equal(t, int64(0), d.Windows[0].Remaining)
}

func TestLimiter_BlocksWhicheverFirst_AllOrNothing(t *testing.T) {
	l, ctx := newLimiter(t)
	base := uuid.NewString()
	p := ratelimit.Policy{Windows: []ratelimit.Limit{
		{Unit: "request", Window: time.Minute, Max: 100},
		{Unit: "token", Window: time.Minute, Max: 1000},
	}}

	d, err := l.Check(ctx, base, p, map[string]int64{"request": 1, "token": 600})
	require.NoError(t, err)
	require.True(t, d.Allowed)

	// token would hit 1200 > 1000 -> blocked on token (whichever first).
	d, err = l.Check(ctx, base, p, map[string]int64{"request": 1, "token": 600})
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Equal(t, "token", d.BlockedUnit)

	// All-or-nothing: the blocked call must NOT have consumed the request counter,
	// so a call that fits both windows still passes.
	d, err = l.Check(ctx, base, p, map[string]int64{"request": 1, "token": 300})
	require.NoError(t, err)
	require.True(t, d.Allowed, "blocked call must not partially consume counters")
}

func TestLimiter_NoWindowsAllows(t *testing.T) {
	l, ctx := newLimiter(t)
	d, err := l.Check(ctx, uuid.NewString(), ratelimit.Policy{}, map[string]int64{"request": 1})
	require.NoError(t, err)
	require.True(t, d.Allowed)
}
