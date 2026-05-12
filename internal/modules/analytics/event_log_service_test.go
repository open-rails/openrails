package analytics

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/spool"
)

func TestLogPaymentEventSpoolsWithDefaultsWhenClickHouseDown(t *testing.T) {
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}

	now := time.Date(2026, 5, 12, 10, 30, 0, 0, time.UTC)
	svc := &EventLogService{
		config: &config.ClickHouseConfig{
			HTTPAddr:   "127.0.0.1:1",
			ClientAddr: "127.0.0.1:1",
			Database:   "analytics",
		},
		spool: sp,
		clock: clockwork.NewFakeClockAt(now),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := svc.LogPaymentEvent(ctx, PaymentEventData{
		UserID:    "user-1",
		EventType: PaymentEventChargeSuccess,
		Processor: "test",
	}); err != nil {
		t.Fatalf("log payment event: %v", err)
	}

	paths, err := sp.List(10)
	if err != nil {
		t.Fatalf("list spool: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("spool files = %d, want 1", len(paths))
	}

	rec, _, err := sp.Read(paths[0])
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	if rec.Kind != "payment" {
		t.Fatalf("kind = %q, want payment", rec.Kind)
	}

	var event PaymentEventData
	if err := json.Unmarshal(rec.Data, &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if event.EventID == uuid.Nil {
		t.Fatal("event ID was not assigned before enqueue")
	}
	if !event.Timestamp.Equal(now) {
		t.Fatalf("timestamp = %s, want %s", event.Timestamp, now)
	}
	if event.Currency != "usd" {
		t.Fatalf("currency = %q, want usd", event.Currency)
	}
}

func TestLogPaymentEventDisabledClickHouseDoesNotSpool(t *testing.T) {
	svc, err := NewEventLogService(&config.ClickHouseConfig{})
	if err != nil {
		t.Fatalf("new event log service: %v", err)
	}
	defer func() { _ = svc.Close() }()

	if err := svc.LogPaymentEvent(context.Background(), PaymentEventData{
		UserID:    "user-1",
		EventType: PaymentEventChargeSuccess,
		Processor: "test",
	}); err != nil {
		t.Fatalf("log payment event: %v", err)
	}
	if svc.spool != nil {
		t.Fatal("disabled ClickHouse service unexpectedly created a spool")
	}
}
