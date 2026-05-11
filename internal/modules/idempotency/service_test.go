package idempotency

import (
	"context"
	"testing"
	"time"
)

func TestTryTakeoverPendingMemory(t *testing.T) {
	ctx := context.Background()
	svc := NewIdempotencyService(nil)

	_, exists, err := svc.Begin(ctx, "webhook.test", "evt_1")
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if exists {
		t.Fatal("expected first begin to create pending record")
	}

	taken, err := svc.TryTakeoverPending(ctx, "webhook.test", "evt_1", time.Hour)
	if err != nil {
		t.Fatalf("takeover fresh pending: %v", err)
	}
	if taken {
		t.Fatal("fresh pending record should not be taken over")
	}

	taken, err = svc.TryTakeoverPending(ctx, "webhook.test", "evt_1", -time.Second)
	if err != nil {
		t.Fatalf("takeover stale pending: %v", err)
	}
	if !taken {
		t.Fatal("stale pending record should be taken over")
	}
}
