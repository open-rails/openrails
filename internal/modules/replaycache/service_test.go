package replaycache

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The in-memory fallback's pending/success/failed lifecycle, driven by a pinned clock.
func TestMemoryStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	svc := NewStoreWithTTL(nil, time.Hour, WithClock(func() time.Time { return now }))
	t.Cleanup(svc.Close)

	rec, exists, err := svc.Begin(ctx, "webhook", "evt_1")
	require.NoError(t, err)
	require.False(t, exists)
	require.Equal(t, StatusPending, rec.Status)

	rec, exists, err = svc.Begin(ctx, "webhook", "evt_1")
	require.NoError(t, err)
	require.True(t, exists, "a concurrent duplicate sees the pending claim")
	require.Equal(t, StatusPending, rec.Status)

	_, exists, err = svc.Begin(ctx, "checkout", "evt_1")
	require.NoError(t, err)
	require.False(t, exists, "operations namespace their keys")

	taken, err := svc.TryTakeoverPending(ctx, "webhook", "evt_1", 10*time.Minute)
	require.NoError(t, err)
	require.False(t, taken, "a fresh holder keeps its claim")

	// A heartbeat keeps a slow holder from looking dead.
	now = now.Add(9 * time.Minute)
	renewed, err := svc.RenewPending(ctx, "webhook", "evt_1")
	require.NoError(t, err)
	require.True(t, renewed)
	now = now.Add(9 * time.Minute)
	taken, err = svc.TryTakeoverPending(ctx, "webhook", "evt_1", 10*time.Minute)
	require.NoError(t, err)
	require.False(t, taken)

	now = now.Add(2 * time.Minute)
	taken, err = svc.TryTakeoverPending(ctx, "webhook", "evt_1", 10*time.Minute)
	require.NoError(t, err)
	require.True(t, taken, "a stale holder is taken over")

	require.NoError(t, svc.Complete(ctx, "webhook", "evt_1", json.RawMessage(`{"ok":true}`)))
	renewed, err = svc.RenewPending(ctx, "webhook", "evt_1")
	require.NoError(t, err)
	require.False(t, renewed, "a late heartbeat must not resurrect a completed record")
	taken, err = svc.TryTakeoverPending(ctx, "webhook", "evt_1", -time.Hour)
	require.NoError(t, err)
	require.False(t, taken)
	rec, err = svc.Get(ctx, "webhook", "evt_1")
	require.NoError(t, err)
	require.Equal(t, StatusSuccess, rec.Status)
	require.JSONEq(t, `{"ok":true}`, string(rec.Result))

	now = now.Add(time.Hour + time.Second)
	rec, err = svc.Get(ctx, "webhook", "evt_1")
	require.NoError(t, err)
	require.Nil(t, rec, "records expire after the TTL")
	_, exists, err = svc.Begin(ctx, "webhook", "evt_1")
	require.NoError(t, err)
	require.False(t, exists)

	// Failures are retryable sooner: they live half the TTL.
	require.NoError(t, svc.Fail(ctx, "webhook", "evt_2", errors.New("declined")))
	rec, err = svc.Get(ctx, "webhook", "evt_2")
	require.NoError(t, err)
	require.Equal(t, StatusFailed, rec.Status)
	require.Equal(t, "declined", rec.Error)
	now = now.Add(31 * time.Minute)
	rec, err = svc.Get(ctx, "webhook", "evt_2")
	require.NoError(t, err)
	require.Nil(t, rec)
}
