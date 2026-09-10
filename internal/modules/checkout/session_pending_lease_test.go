package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/replaycache"
)

// xs-007 row 39: the pending-create lease is measured from the last
// heartbeat. A holder still inside its provider call renews it, so a second
// create with the same key cannot take the key over — the duplicate-create
// race the 30 s clock used to open. A holder that stops beating (dead) is
// taken over after one lease of silence.
func TestCheckoutSessionPendingLease_HeartbeatKeepsALiveHolder(t *testing.T) {
	ctx := context.Background()
	clock := clockwork.NewFakeClockAt(time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC))
	// In-memory backend, with the same application clock as the service.
	store := replaycache.NewStore(nil, replaycache.WithClock(clock.Now))
	t.Cleanup(store.Close)
	const lease = 200 * time.Millisecond
	svc := &CheckoutSessionService{idempotencyService: store, pendingLease: lease, clock: clock}
	const key = "user:1:create-abc"

	_, exists, err := store.Begin(ctx, checkoutSessionIdempotencyOp, key)
	require.NoError(t, err)
	require.False(t, exists)

	stop := svc.startPendingHeartbeat(ctx, key)
	require.NoError(t, clock.BlockUntilContext(t.Context(), 1))
	rec, err := store.Get(ctx, checkoutSessionIdempotencyOp, key)
	require.NoError(t, err)
	require.NotNil(t, rec)
	lastBeat := rec.CreatedAt
	for range 16 {
		clock.Advance(lease / 4)
		require.Eventually(t, func() bool {
			rec, getErr := store.Get(ctx, checkoutSessionIdempotencyOp, key)
			if getErr != nil || rec == nil || !rec.CreatedAt.After(lastBeat) {
				return false
			}
			lastBeat = rec.CreatedAt
			return true
		}, time.Second, time.Millisecond, "heartbeat must renew the pending record")
	}
	taken, err := store.TryTakeoverPending(ctx, checkoutSessionIdempotencyOp, key, lease)
	require.NoError(t, err)
	require.False(t, taken, "a beating holder is never taken over, however long the provider takes")

	stop() // the holder dies
	clock.Advance(2 * lease)
	taken, err = store.TryTakeoverPending(ctx, checkoutSessionIdempotencyOp, key, lease)
	require.NoError(t, err)
	require.True(t, taken, "one lease of silence from a dead holder frees the key")
}
