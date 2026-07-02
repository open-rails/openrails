//go:build integration

package idempotency

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

// #678: lease renewal + takeover semantics against real Redis (the production store).
func TestRenewAndTakeoverPendingRedis(t *testing.T) {
	rdb, ctx := dbtest.SharedRedisClient(t)
	svc := NewIdempotencyServiceWithTTL(rdb, time.Hour)
	defer svc.Close()

	_, existed, err := svc.Begin(ctx, "webhook.test", "evt_redis")
	require.NoError(t, err)
	require.False(t, existed)

	// Fresh pending: not stale, no takeover.
	taken, err := svc.TryTakeoverPending(ctx, "webhook.test", "evt_redis", time.Hour)
	require.NoError(t, err)
	require.False(t, taken)

	// Heartbeat renews.
	time.Sleep(10 * time.Millisecond)
	renewed, err := svc.RenewPending(ctx, "webhook.test", "evt_redis")
	require.NoError(t, err)
	require.True(t, renewed)
	rec, err := svc.Get(ctx, "webhook.test", "evt_redis")
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.Less(t, time.Since(rec.CreatedAt), 500*time.Millisecond)

	// Aged-out pending is taken over.
	time.Sleep(20 * time.Millisecond)
	taken, err = svc.TryTakeoverPending(ctx, "webhook.test", "evt_redis", 10*time.Millisecond)
	require.NoError(t, err)
	require.True(t, taken)

	// Complete wins; late heartbeat cannot resurrect it.
	require.NoError(t, svc.Complete(ctx, "webhook.test", "evt_redis", nil))
	renewed, err = svc.RenewPending(ctx, "webhook.test", "evt_redis")
	require.NoError(t, err)
	require.False(t, renewed)
	rec, err = svc.Get(ctx, "webhook.test", "evt_redis")
	require.NoError(t, err)
	require.Equal(t, IdempotencyStatusSuccess, rec.Status)
}
