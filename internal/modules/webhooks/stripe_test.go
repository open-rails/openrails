package webhooks

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
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

func TestStripeInvoicePeriodStart(t *testing.T) {
	raw := []byte(`{
		"id":"in_1",
		"subscription":"sub_1",
		"lines":{"data":[
			{"period":{"start":100,"end":200},"price":{"id":"price_1"}},
			{"period":{"start":50,"end":150},"price":{"id":"price_2"}}
		]}
	}`)
	var inv stripeInvoice
	if err := json.Unmarshal(raw, &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	start := stripeInvoicePeriodStart(inv)
	if start.IsZero() {
		t.Fatalf("expected non-zero period start")
	}
	if start.Unix() != 50 {
		t.Fatalf("expected unix=50, got %d", start.Unix())
	}
}

func TestStripeInvoiceEffectiveMetadataUsesSubscriptionDetailsFallback(t *testing.T) {
	inv := stripeInvoice{
		Metadata: map[string]string{"user_id": "invoice_user"},
	}
	inv.SubscriptionDetails.Metadata = map[string]string{
		"user_id":  "subscription_user",
		"price_id": "price_123",
	}

	metadata := stripeInvoiceEffectiveMetadata(inv)
	if metadata["user_id"] != "invoice_user" {
		t.Fatalf("expected invoice metadata to win, got %q", metadata["user_id"])
	}
	if metadata["price_id"] != "price_123" {
		t.Fatalf("expected subscription_details metadata fallback")
	}
}

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

	md := stripeInvoiceEffectiveMetadata(inv)
	if md["user_id"] != "019e51df-17aa-7de4-a4e9-4f2e7c33ea29" {
		t.Fatalf("user_id not resolved from parent.subscription_details.metadata: %q", md["user_id"])
	}
	if md["internal_price_id"] != "4ecb4c2b-b057-49c0-a691-10639c0969db" {
		t.Fatalf("internal_price_id not resolved: %q", md["internal_price_id"])
	}
	if got := inv.Lines.Data[0].priceID(); got != "price_stripe_initiate" {
		t.Fatalf("priceID() from pricing.price_details.price = %q, want price_stripe_initiate", got)
	}
	// On the preview shape there is no top-level subscription; it lives under
	// parent.subscription_details.subscription. handleInvoicePaid bails ("missing
	// subscription id") without this fallback — the root cause of purchases
	// sticking on Free.
	if got := inv.processorSubscriptionID(); got != "sub_test" {
		t.Fatalf("processorSubscriptionID() from parent = %q, want sub_test", got)
	}
}

func TestStripeInvoiceProcessorSubscriptionID(t *testing.T) {
	// Classic/snapshot shape: top-level subscription is used.
	classic := stripeInvoice{Subscription: "sub_classic"}
	if got := classic.processorSubscriptionID(); got != "sub_classic" {
		t.Fatalf("classic: got %q, want sub_classic", got)
	}

	// 2026-04-22.preview shape: subscription only under parent.subscription_details.
	var preview stripeInvoice
	if err := json.Unmarshal([]byte(`{
      "parent": {"subscription_details": {"subscription": "sub_preview"}}
    }`), &preview); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := preview.processorSubscriptionID(); got != "sub_preview" {
		t.Fatalf("preview: got %q, want sub_preview", got)
	}

	// Top-level wins when both are present.
	both := stripeInvoice{Subscription: "sub_top"}
	both.Parent.SubscriptionDetails.Subscription = "sub_parent"
	if got := both.processorSubscriptionID(); got != "sub_top" {
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

func TestValidateStripeInvoicePrice(t *testing.T) {
	price := &models.Price{Amount: 2399, Currency: "USD"}
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 2399, Currency: "usd"}, price); err != nil {
		t.Fatalf("expected valid invoice: %v", err)
	}
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 2300, Currency: "usd"}, price); err == nil || !strings.Contains(err.Error(), "amount mismatch") {
		t.Fatalf("expected amount mismatch, got %v", err)
	}
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 0, Currency: "usd"}, price); err != nil {
		t.Fatalf("expected zero-amount trial invoice without charge to be valid: %v", err)
	}
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 0, PaymentIntent: "pi_123", Currency: "usd"}, price); err == nil || !strings.Contains(err.Error(), "amount mismatch") {
		t.Fatalf("expected zero paid invoice with payment intent to fail amount validation, got %v", err)
	}
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 2399, Currency: "eur"}, price); err == nil || !strings.Contains(err.Error(), "currency mismatch") {
		t.Fatalf("expected currency mismatch, got %v", err)
	}

	// Model B upgrade: billing_reason=subscription_update invoices carry a
	// PRORATED amount (new_full - old_unused) that never equals the new tier's
	// list price. They must be accepted as long as currency matches and the
	// settled amount is positive (Stripe is the source of truth for the amount).
	gm := &models.Price{Amount: 11900, Currency: "USD"} // Grandmaster list $119.00
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 6001, Currency: "usd", BillingReason: "subscription_update", Charge: "ch_up"}, gm); err != nil {
		t.Fatalf("expected prorated upgrade invoice (6001 != 11900) to be accepted: %v", err)
	}
	// Currency still validated on proration invoices.
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 6001, Currency: "eur", BillingReason: "subscription_update"}, gm); err == nil || !strings.Contains(err.Error(), "currency mismatch") {
		t.Fatalf("expected currency mismatch on proration invoice, got %v", err)
	}
	// A non-positive proration amount is still rejected (nothing was collected).
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 0, Currency: "usd", BillingReason: "subscription_update"}, gm); err == nil || !strings.Contains(err.Error(), "non-positive") {
		t.Fatalf("expected non-positive proration amount to fail, got %v", err)
	}
	// Renewals (subscription_cycle) keep the strict exact list-price match.
	if err := validateStripeInvoicePrice(stripeInvoice{AmountPaid: 6001, Currency: "usd", BillingReason: "subscription_cycle"}, gm); err == nil || !strings.Contains(err.Error(), "amount mismatch") {
		t.Fatalf("expected renewal invoice to keep exact amount match, got %v", err)
	}
}

func TestParseCheckoutSessionIDUsesEffectiveMetadata(t *testing.T) {
	checkoutID := "cs_0189f3e6-62c6-77b6-9ae9-c17ecb90f466"
	inv := stripeInvoice{}
	inv.SubscriptionDetails.Metadata = map[string]string{"checkout_session_id": checkoutID}

	metadata := stripeInvoiceEffectiveMetadata(inv)
	if got := parseCheckoutSessionID(metadata); got == uuid.Nil {
		t.Fatalf("expected checkout session id from subscription_details metadata")
	}
}

func TestWebhookDispatcher_RejectsUnsignedStripeJob(t *testing.T) {
	d := &WebhookDispatcher{}
	err := d.Process(context.Background(), &WebhookMessage{
		Processor: "stripe",
		Payload:   []byte(`{"id":"evt_1","type":"invoice.paid","data":{"object":{}}}`),
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
		Processor:      "stripe",
		Payload:        []byte(`{"id":"evt_1","type":"unhandled.event","data":{"object":{}}}`),
		SignatureValid: &verified,
	})
	if err != nil {
		t.Fatalf("expected previously verified stripe payload to process without dispatcher re-verification: %v", err)
	}
}

func TestApplyStripeSubscriptionStatus(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	futureEnd := now.Add(48 * time.Hour)

	tests := []struct {
		name              string
		status            string
		currentPeriodEnds *time.Time
		expectedStatus    models.SubscriptionStatus
		expectCancelledAt bool
		expectEndedAt     bool
		expectedEndedAt   *time.Time
	}{
		{name: "active", status: "active", expectedStatus: models.StatusActive},
		{name: "trialing", status: "trialing", expectedStatus: models.StatusActive},
		{name: "past due", status: "past_due", expectedStatus: models.StatusPastDue},
		{name: "unpaid", status: "unpaid", expectedStatus: models.StatusPastDue},
		{name: "incomplete", status: "incomplete", expectedStatus: models.StatusPastDue},
		{name: "canceled uses period end", status: "canceled", currentPeriodEnds: &futureEnd, expectedStatus: models.StatusCancelled, expectCancelledAt: true, expectEndedAt: true, expectedEndedAt: &futureEnd},
		{name: "incomplete expired uses now", status: "incomplete_expired", expectedStatus: models.StatusCancelled, expectCancelledAt: true, expectEndedAt: true, expectedEndedAt: &now},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub := &models.Subscription{Status: models.StatusActive, CurrentPeriodEndsAt: tt.currentPeriodEnds}
			applyStripeSubscriptionStatus(sub, tt.status, now)

			if sub.Status != tt.expectedStatus {
				t.Fatalf("expected status %s, got %s", tt.expectedStatus, sub.Status)
			}
			if (sub.CancelledAt != nil) != tt.expectCancelledAt {
				t.Fatalf("expected CancelledAt presence %v, got %v", tt.expectCancelledAt, sub.CancelledAt != nil)
			}
			if (sub.EndedAt != nil) != tt.expectEndedAt {
				t.Fatalf("expected EndedAt presence %v, got %v", tt.expectEndedAt, sub.EndedAt != nil)
			}
			if tt.expectedEndedAt != nil {
				if !sub.EndedAt.Equal(*tt.expectedEndedAt) {
					t.Fatalf("expected EndedAt %v, got %v", *tt.expectedEndedAt, *sub.EndedAt)
				}
			}
		})
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

func TestApplyStripeSubscriptionStatus_CancelledClearsRetrySchedule(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	attempts := 2
	sub := &models.Subscription{
		Status:        models.StatusPastDue,
		LastRetryAt:   &now,
		RetryAttempts: &attempts,
		NextRetryAt:   &now,
		GraceEndsAt:   &now,
	}

	applyStripeSubscriptionStatus(sub, "canceled", now)

	if sub.Status != models.StatusCancelled {
		t.Fatalf("expected cancelled, got %s", sub.Status)
	}
	if sub.LastRetryAt != nil || sub.RetryAttempts != nil || sub.NextRetryAt != nil || sub.GraceEndsAt != nil {
		t.Fatalf("expected retry schedule fields to be cleared")
	}
}

func TestApplyStripeScheduledCancellationKeepsFuturePeriodActive(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	futureEnd := now.Add(48 * time.Hour)
	sub := &models.Subscription{Status: models.StatusActive, CurrentPeriodEndsAt: &futureEnd}

	applyStripeScheduledCancellation(sub, "active", now.Add(-time.Hour).Unix(), now)

	if sub.Status != models.StatusActive {
		t.Fatalf("expected paid-through scheduled cancellation to remain active, got %s", sub.Status)
	}
	if sub.CancelType == nil || *sub.CancelType != models.CancelTypeUser {
		t.Fatalf("expected user cancel type, got %v", sub.CancelType)
	}
	if sub.CancelledAt == nil || !sub.CancelledAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("expected canceled_at from stripe, got %v", sub.CancelledAt)
	}
	if sub.EndedAt == nil || !sub.EndedAt.Equal(futureEnd) {
		t.Fatalf("expected ended_at at period end, got %v", sub.EndedAt)
	}
}

func TestApplyStripeScheduledCancellationCancelsEndedPeriod(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	pastEnd := now.Add(-time.Hour)
	sub := &models.Subscription{Status: models.StatusActive, CurrentPeriodEndsAt: &pastEnd}

	applyStripeScheduledCancellation(sub, "active", 0, now)

	if sub.Status != models.StatusCancelled {
		t.Fatalf("expected ended scheduled cancellation to be cancelled, got %s", sub.Status)
	}
	if sub.EndedAt == nil || !sub.EndedAt.Equal(now) {
		t.Fatalf("expected ended_at now, got %v", sub.EndedAt)
	}
}

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
