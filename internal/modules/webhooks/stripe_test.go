package webhooks

import (
	"context"
	"encoding/json"
	"testing"
)

// Regression for the 2026-04-22.preview invoice shape: subscription metadata
// lives under parent.subscription_details and the line-item price under
// pricing.price_details.price (legacy subscription_details / price.id absent).
func TestStripeInvoicePreviewShapeParsing(t *testing.T) {
	payload := []byte(`{
      "id": "in_test",
      "customer": "cus_test",
      "amount_paid": 1200,
      "currency": "usd",
      "metadata": {},
      "lines": {"data": [{"pricing": {"price_details": {"price": "price_stripe_initiate"}}}]},
      "parent": {"subscription_details": {
        "subscription": "sub_test",
        "metadata": {
          "user_id": "019e51df-17aa-7de4-a4e9-4f2e7c33ea29",
          "internal_price_id": "4ecb4c2b-b057-49c0-a691-10639c0969db"
        }
      }}
    }`)

	var inv stripeInvoice
	if err := json.Unmarshal(payload, &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got := inv.Parent.SubscriptionDetails.Metadata["user_id"]; got != "019e51df-17aa-7de4-a4e9-4f2e7c33ea29" {
		t.Fatalf("user_id not parsed from parent.subscription_details.metadata: %q", got)
	}
	if got := inv.Lines.Data[0].priceID(); got != "price_stripe_initiate" {
		t.Fatalf("priceID() from pricing.price_details.price = %q, want price_stripe_initiate", got)
	}
	// On the preview shape there is no top-level subscription; it lives under
	// parent.subscription_details.subscription. The #684 dirty-mark parse needs
	// this fallback or preview events cannot identify the dirty subscription.
	if got := inv.railSubscriptionID(); got != "sub_test" {
		t.Fatalf("railSubscriptionID() from parent = %q, want sub_test", got)
	}
}

func TestStripeInvoiceRailSubscriptionID(t *testing.T) {
	// Classic/snapshot shape: top-level subscription is used.
	classic := stripeInvoice{Subscription: "sub_classic"}
	if got := classic.railSubscriptionID(); got != "sub_classic" {
		t.Fatalf("classic: got %q, want sub_classic", got)
	}

	// 2026-04-22.preview shape: subscription only under parent.subscription_details.
	var preview stripeInvoice
	if err := json.Unmarshal([]byte(`{
      "parent": {"subscription_details": {"subscription": "sub_preview"}}
    }`), &preview); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := preview.railSubscriptionID(); got != "sub_preview" {
		t.Fatalf("preview: got %q, want sub_preview", got)
	}

	// Top-level wins when both are present.
	both := stripeInvoice{Subscription: "sub_top"}
	both.Parent.SubscriptionDetails.Subscription = "sub_parent"
	if got := both.railSubscriptionID(); got != "sub_top" {
		t.Fatalf("both: got %q, want sub_top (top-level wins)", got)
	}
}

func TestStripeChargeSnapshotTransactionIDsIncludesInvoice(t *testing.T) {
	ids := stripeChargeSnapshotTransactionIDs(stripeCharge{ID: " ch_1 ", PaymentIntent: " pi_1 ", Invoice: " in_1 "})
	want := []string{"ch_1", "pi_1", "in_1"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v", ids, want)
		}
	}
}

func TestStripeInvoicePaymentPaidPreviewShape(t *testing.T) {
	var invoicePayment stripeInvoicePayment
	if err := json.Unmarshal([]byte(`{
        "id": "inpay_1",
        "invoice": "in_1",
        "payment": {"type": "payment_intent", "payment_intent": "pi_1"}
    }`), &invoicePayment); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if invoicePayment.Invoice != "in_1" {
		t.Fatalf("invoice = %q, want in_1", invoicePayment.Invoice)
	}
	if invoicePayment.Payment.PaymentIntent != "pi_1" {
		t.Fatalf("payment_intent = %q, want pi_1", invoicePayment.Payment.PaymentIntent)
	}

	svc := &StripeWebhookService{}
	if err := svc.handleInvoicePaymentPaid(context.Background(), []byte(`{"invoice":"in_1","payment":{"payment_intent":"pi_1"}}`)); err != nil {
		t.Fatalf("expected nil-db handler to be a no-op, got %v", err)
	}
}

func TestWebhookDispatcher_RejectsUnsignedStripeJob(t *testing.T) {
	d := &WebhookDispatcher{}
	err := d.Process(context.Background(), &WebhookMessage{
		Rail:    "stripe",
		Payload: []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{}}}`),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsWebhookErrorNonRetryable(err) {
		t.Fatalf("expected non-retryable error, got %v", err)
	}
}

func TestWebhookDispatcher_UsesPreviouslyVerifiedStripePayload(t *testing.T) {
	verified := true
	d := &WebhookDispatcher{}
	err := d.Process(context.Background(), &WebhookMessage{
		Rail:           "stripe",
		Payload:        []byte(`{"id":"evt_1","type":"unhandled.event","data":{"object":{}}}`),
		SignatureValid: &verified,
	})
	if err != nil {
		t.Fatalf("expected previously verified stripe payload to process without dispatcher re-verification: %v", err)
	}
}

func TestStripeRefundSucceeded(t *testing.T) {
	tests := []struct {
		status string
		want   bool
	}{
		{status: "succeeded", want: true},
		{status: " Succeeded ", want: true},
		{status: "pending", want: false},
		{status: "failed", want: false},
		{status: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := stripeRefundSucceeded(tt.status); got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestStripeDisputeShouldReverse(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		status    string
		want      bool
	}{
		{name: "created needs response", eventType: "charge.dispute.created", status: "needs_response", want: true},
		{name: "created empty status is active", eventType: "charge.dispute.created", status: "", want: true},
		{name: "created won ignored", eventType: "charge.dispute.created", status: "won", want: false},
		{name: "closed lost", eventType: "charge.dispute.closed", status: "lost", want: true},
		{name: "closed won", eventType: "charge.dispute.closed", status: "won", want: false},
		{name: "closed under review", eventType: "charge.dispute.closed", status: "under_review", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripeDisputeShouldReverse(tt.eventType, tt.status); got != tt.want {
				t.Fatalf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func TestStripeDisputeWon(t *testing.T) {
	if !stripeDisputeWon("charge.dispute.closed", "won") {
		t.Fatal("expected won closed dispute to be detected")
	}
	if stripeDisputeWon("charge.dispute.created", "won") {
		t.Fatal("created dispute should not be treated as won recovery")
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
