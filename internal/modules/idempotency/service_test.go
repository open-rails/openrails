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

func TestRenewPendingMemory(t *testing.T) {
	ctx := context.Background()
	svc := NewIdempotencyService(nil)

	if _, _, err := svc.Begin(ctx, "webhook.test", "evt_hb"); err != nil {
		t.Fatalf("begin: %v", err)
	}

	time.Sleep(time.Millisecond)
	renewed, err := svc.RenewPending(ctx, "webhook.test", "evt_hb")
	if err != nil {
		t.Fatalf("renew pending: %v", err)
	}
	if !renewed {
		t.Fatal("pending record should renew")
	}
	rec, err := svc.Get(ctx, "webhook.test", "evt_hb")
	if err != nil || rec == nil {
		t.Fatalf("get: %v", err)
	}
	if time.Since(rec.CreatedAt) > 500*time.Millisecond {
		t.Fatal("renewal should refresh CreatedAt")
	}

	// Completed records must not be resurrected to pending by a late heartbeat.
	if err := svc.Complete(ctx, "webhook.test", "evt_hb", nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	renewed, err = svc.RenewPending(ctx, "webhook.test", "evt_hb")
	if err != nil {
		t.Fatalf("renew after complete: %v", err)
	}
	if renewed {
		t.Fatal("completed record must not be renewed")
	}
	rec, _ = svc.Get(ctx, "webhook.test", "evt_hb")
	if rec == nil || rec.Status != IdempotencyStatusSuccess {
		t.Fatal("completed record must stay success")
	}
}
