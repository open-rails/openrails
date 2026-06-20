//go:build integration

package abuse_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/abuse"
	"github.com/open-rails/openrails/internal/modules/ratelimit"
	"github.com/stretchr/testify/require"
)

func newVelocity(t *testing.T) (*abuse.VelocityGuard, context.Context) {
	t.Helper()
	rdb, ctx := dbtest.SharedRedisClient(t)
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
