package analytics

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/spool"
)

func TestLogPaymentEventSpoolsWithoutCurrencyDefaultWhenClickHouseDown(t *testing.T) {
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

	ctx, cancel := context.WithTimeout(dbtest.WithTestMerchant(context.Background()), 50*time.Millisecond)
	defer cancel()
	if err := svc.LogPaymentEvent(ctx, PaymentEventData{
		UserID:    "user-1",
		EventType: PaymentEventChargeSuccess,
		Rail:      "test",
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
	if event.Currency != "" {
		t.Fatalf("currency = %q, want blank", event.Currency)
	}
}

func TestLogPaymentEventRequiresCurrencyWithAmount(t *testing.T) {
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	amount := 1.23
	svc := &EventLogService{
		config: &config.ClickHouseConfig{HTTPAddr: "127.0.0.1:1", ClientAddr: "127.0.0.1:1", Database: "analytics"},
		spool:  sp,
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 5, 12, 10, 30, 0, 0, time.UTC)),
	}
	ctx, cancel := context.WithTimeout(dbtest.WithTestMerchant(context.Background()), 50*time.Millisecond)
	defer cancel()
	err = svc.LogPaymentEvent(ctx, PaymentEventData{UserID: "user-1", EventType: PaymentEventChargeSuccess, Rail: "test", Amount: &amount})
	if err == nil {
		t.Fatal("expected missing currency error")
	}
}

// TestLogPaymentEventStampsResolvedMerchant proves the spooled analytics event is
// merchant-scoped to the merchant resolved on the context (issue #232).
func TestLogPaymentEventStampsResolvedMerchant(t *testing.T) {
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	svc := &EventLogService{
		config: &config.ClickHouseConfig{HTTPAddr: "127.0.0.1:1", ClientAddr: "127.0.0.1:1", Database: "analytics"},
		spool:  sp,
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 5, 12, 10, 30, 0, 0, time.UTC)),
	}

	ctx, cancel := context.WithTimeout(dbtest.WithTestMerchant(context.Background()), 50*time.Millisecond)
	defer cancel()
	if err := svc.LogPaymentEvent(ctx, PaymentEventData{UserID: "user-1", EventType: PaymentEventChargeSuccess, Rail: "test"}); err != nil {
		t.Fatalf("log payment event: %v", err)
	}

	event := readOnlySpooledPayment(t, sp)
	if event.MerchantID != dbtest.TestMerchantID.String() {
		t.Fatalf("merchant_id = %q, want %q", event.MerchantID, dbtest.TestMerchantID.String())
	}
}

// TestLogPaymentEventCarriesResolvedMerchant proves the event is scoped to the
// merchant resolved onto the request context, not the default (issue #232).
func TestLogPaymentEventCarriesResolvedMerchant(t *testing.T) {
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	svc := &EventLogService{
		config: &config.ClickHouseConfig{HTTPAddr: "127.0.0.1:1", ClientAddr: "127.0.0.1:1", Database: "analytics"},
		spool:  sp,
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 5, 12, 10, 30, 0, 0, time.UTC)),
	}

	merchantID := merchant.ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	ctx, cancel := context.WithTimeout(merchant.WithID(context.Background(), merchantID), 50*time.Millisecond)
	defer cancel()
	if err := svc.LogPaymentEvent(ctx, PaymentEventData{UserID: "user-1", EventType: PaymentEventChargeSuccess, Rail: "test"}); err != nil {
		t.Fatalf("log payment event: %v", err)
	}

	event := readOnlySpooledPayment(t, sp)
	if event.MerchantID != merchantID.String() {
		t.Fatalf("merchant_id = %q, want %q", event.MerchantID, merchantID.String())
	}
}

// TestLogSubscriptionEventStampsMerchant proves subscription events are merchant-scoped.
func TestLogSubscriptionEventStampsMerchant(t *testing.T) {
	sp, err := spool.New(t.TempDir())
	if err != nil {
		t.Fatalf("create spool: %v", err)
	}
	svc := &EventLogService{
		config: &config.ClickHouseConfig{HTTPAddr: "127.0.0.1:1", ClientAddr: "127.0.0.1:1", Database: "analytics"},
		spool:  sp,
		clock:  clockwork.NewFakeClockAt(time.Date(2026, 5, 12, 10, 30, 0, 0, time.UTC)),
	}

	ctx, cancel := context.WithTimeout(dbtest.WithTestMerchant(context.Background()), 50*time.Millisecond)
	defer cancel()
	if err := svc.LogSubscriptionEvent(ctx, SubscriptionEventData{SubscriptionID: uuid.New(), UserID: "user-1", EventType: PaymentEventSubscriptionCreated, Rail: "test"}); err != nil {
		t.Fatalf("log subscription event: %v", err)
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
	var event SubscriptionEventData
	if err := json.Unmarshal(rec.Data, &event); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if event.MerchantID != dbtest.TestMerchantID.String() {
		t.Fatalf("merchant_id = %q, want %q", event.MerchantID, dbtest.TestMerchantID.String())
	}
}

func readOnlySpooledPayment(t *testing.T, sp *spool.Spool) PaymentEventData {
	t.Helper()
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
	return event
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
		Rail:      "test",
	}); err != nil {
		t.Fatalf("log payment event: %v", err)
	}
	if svc.spool != nil {
		t.Fatal("disabled ClickHouse service unexpectedly created a spool")
	}
}

// TestEventLogIsOptionalNoOpWhenUnconfigured proves the issue #232 invariant
// that ClickHouse is OPTIONAL: a billing operation that emits analytics must
// succeed (no error on the hot path) when ClickHouse is unconfigured, whether
// the sink is a nil *EventLogService or one built from empty config. This is
// what guarantees billing/credit/entitlement correctness never depends on
// ClickHouse availability.
func TestEventLogIsOptionalNoOpWhenUnconfigured(t *testing.T) {
	ctx := context.Background()

	// Case 1: nil sink (analytics disabled entirely; build_runtime leaves the
	// field nil when init fails). Every Log* method must tolerate a nil receiver.
	var nilSvc *EventLogService
	assertSinkNoOp(t, ctx, nilSvc)

	// Case 2: unconfigured sink (empty ClickHouse config).
	svc, err := NewEventLogService(&config.ClickHouseConfig{})
	if err != nil {
		t.Fatalf("new event log service: %v", err)
	}
	defer func() { _ = svc.Close() }()
	assertSinkNoOp(t, ctx, svc)
}

func assertSinkNoOp(t *testing.T, ctx context.Context, svc *EventLogService) {
	t.Helper()
	if err := svc.LogPaymentEvent(ctx, PaymentEventData{UserID: "u", EventType: PaymentEventChargeSuccess, Rail: "p"}); err != nil {
		t.Fatalf("LogPaymentEvent errored on unconfigured sink: %v", err)
	}
	if err := svc.LogSubscriptionEvent(ctx, SubscriptionEventData{SubscriptionID: uuid.New(), UserID: "u", EventType: PaymentEventSubscriptionCreated, Rail: "p"}); err != nil {
		t.Fatalf("LogSubscriptionEvent errored on unconfigured sink: %v", err)
	}
	if err := svc.LogTransactionEvent(ctx, TransactionEventData{TransactionID: "t", EventType: "charge", Rail: "p"}); err != nil {
		t.Fatalf("LogTransactionEvent errored on unconfigured sink: %v", err)
	}
	if err := svc.LogACUEvent(ctx, ACUEventData{EventType: "acu", Rail: "p"}); err != nil {
		t.Fatalf("LogACUEvent errored on unconfigured sink: %v", err)
	}
	if err := svc.LogChargebackEvent(ctx, ChargebackEventData{ChargebackID: "c", EventType: "chargeback", Rail: "p"}); err != nil {
		t.Fatalf("LogChargebackEvent errored on unconfigured sink: %v", err)
	}
}
