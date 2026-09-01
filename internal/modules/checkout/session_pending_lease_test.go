package checkout

import (
	"context"
	"testing"
	"time"

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
	store := replaycache.NewStore(nil) // in-memory backend, the same code path
	const lease = 200 * time.Millisecond
	svc := &CheckoutSessionService{idempotencyService: store, pendingLease: lease}
	const key = "user:1:create-abc"

	_, exists, err := store.Begin(ctx, checkoutSessionIdempotencyOp, key)
	require.NoError(t, err)
	require.False(t, exists)

	stop := svc.startPendingHeartbeat(ctx, key)
	time.Sleep(4 * lease) // several lease lengths inside the provider call
	taken, err := store.TryTakeoverPending(ctx, checkoutSessionIdempotencyOp, key, lease)
	require.NoError(t, err)
	require.False(t, taken, "a beating holder is never taken over, however long the provider takes")

	stop() // the holder dies
	time.Sleep(2 * lease)
	taken, err = store.TryTakeoverPending(ctx, checkoutSessionIdempotencyOp, key, lease)
	require.NoError(t, err)
	require.True(t, taken, "one lease of silence from a dead holder frees the key")
}
