package webhooks

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFailedPaymentTransactionID(t *testing.T) {
	cases := []struct {
		name string
		inv  stripeInvoice
		want string
	}{
		{"prefers charge", stripeInvoice{ID: "in_1", Charge: "ch_1", PaymentIntent: "pi_1"}, "failed:ch_1"},
		{"falls back to payment_intent", stripeInvoice{ID: "in_1", PaymentIntent: "pi_1"}, "failed:pi_1"},
		{"falls back to invoice id", stripeInvoice{ID: "in_1"}, "failed:in_1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := failedPaymentTransactionID(tc.inv); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			// Must never collide with a success row (which uses the bare id).
			if !strings.HasPrefix(failedPaymentTransactionID(tc.inv), "failed:") {
				t.Error("failed transaction id must be prefixed to avoid colliding with the success charge")
			}
		})
	}
}

func TestFailedInvoiceParsesAmountDue(t *testing.T) {
	var inv stripeInvoice
	if err := json.Unmarshal([]byte(`{"id":"in_1","subscription":"sub_1","amount_due":1200,"amount_paid":0,"currency":"usd","payment_intent":"pi_1"}`), &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if inv.AmountDue != 1200 {
		t.Errorf("amount_due = %d, want 1200", inv.AmountDue)
	}
	if inv.AmountPaid != 0 {
		t.Errorf("amount_paid = %d, want 0", inv.AmountPaid)
	}
}
