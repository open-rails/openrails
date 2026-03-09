package services

import (
	"encoding/json"
	"testing"
)

func TestStripeInvoicePeriodEnd(t *testing.T) {
	raw := []byte(`{
		"id":"in_1",
		"subscription":"sub_1",
		"lines":{"data":[
			{"period":{"start":1,"end":100},"price":{"id":"price_1"}},
			{"period":{"start":2,"end":200},"price":{"id":"price_2"}}
		]}
	}`)
	var inv stripeInvoice
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	end := stripeInvoicePeriodEnd(inv)
	if end.IsZero() {
		t.Fatalf("expected non-zero period end")
	}
	if end.Unix() != 200 {
		t.Fatalf("expected unix=200, got %d", end.Unix())
	}
}
