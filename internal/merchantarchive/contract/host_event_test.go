package contract

import "testing"

func TestPaymentHostEventStructuredDedupeKey(t *testing.T) {
	// The first 16 digits pass Luhn, but this is a canonical v4 UUID inside
	// the exact dedupe key written by enqueue_payment_settlement_event.
	payment := "41111111-1111-4115-a111-111111111111"
	event, dedupe, delivered := "payment.settled", "payment:"+payment, "2026-09-18 00:00:00+00"
	p := Profile{Name: "host_outbox", Columns: []Column{{"event_type", "text"}, {"payment_id", "uuid"}, {"dedupe_key", "text"}, {"delivered_at", "timestamptz"}}}
	if err := ValidateValues(p, []*string{&event, &payment, &dedupe, &delivered}); err != nil {
		t.Fatalf("refused typed payment dedupe UUID: %v", err)
	}
	for _, bad := range []string{
		"4111111111111111", "payment:4111111111111111",
		"payment:" + payment + ":4111111111111111",
		"payment:41111111-1111-4115-a111-111111111112",
		"payment:10000000-0000-0000-0000-000000000002",
		"unrelated-safe-key",
	} {
		if ValidateValues(p, []*string{&event, &payment, &bad, &delivered}) == nil {
			t.Errorf("accepted unsafe or mismatched dedupe key %q", bad)
		}
	}
	event = "delinquency.entered"
	if ValidateValues(p, []*string{&event, &payment, &dedupe, &delivered}) == nil {
		t.Error("accepted payment UUID exemption for another event type")
	}
	// The key is a UUID-derived id, so the card scan reads it as one wherever
	// it appears. What confines it to its own row is the coordinate check
	// above, not a card-shaped accident.
	if !safeText(dedupe) {
		t.Error("a UUID-derived id must not read as card data")
	}
	if validateJSON("payments.metadata", `{"order_id":"`+dedupe+`"}`) != nil {
		t.Error("a UUID-derived id must not read as card data in metadata")
	}
	if validateJSON("payments.metadata", `{"order_id":"4111111111111111"}`) == nil {
		t.Error("accepted a card number in metadata")
	}
	event, payment = "payment.settled", testMerchant
	dedupe = "payment:" + payment
	if err := ValidateValues(p, []*string{&event, &payment, &dedupe, &delivered}); err != nil {
		t.Fatalf("refused ordinary matching payment key: %v", err)
	}
}
