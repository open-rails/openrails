//go:build integration

package abuse_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func newVelocity(t *testing.T) (*abuse.VelocityGuard, context.Context) {
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
	return abuse.NewVelocityGuard(ratelimit.NewLimiter(rdb)), ctx
}

func TestVelocity_SignupCapPerInstrument(t *testing.T) {
	g, ctx := newVelocity(t)
	fp := uuid.NewString()
	for i := 0; i < 3; i++ {
		ok, err := g.AllowSignup(ctx, fp, 3, time.Hour)
		require.NoError(t, err)
		require.True(t, ok, "signup %d under cap", i+1)
	}
	ok, err := g.AllowSignup(ctx, fp, 3, time.Hour)
	require.NoError(t, err)
	require.False(t, ok, "4th signup on same instrument over cap=3 denied")

	// A different instrument is independent.
	ok, err = g.AllowSignup(ctx, uuid.NewString(), 3, time.Hour)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestVelocity_DeclineStorm(t *testing.T) {
	g, ctx := newVelocity(t)
	card := uuid.NewString()
	for i := 0; i < 2; i++ {
		ok, err := g.AllowChargeAttempt(ctx, card, 2, time.Minute)
		require.NoError(t, err)
		require.True(t, ok)
	}
	ok, err := g.AllowChargeAttempt(ctx, card, 2, time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "3rd charge attempt on the same card over cap=2 throttled")
}
