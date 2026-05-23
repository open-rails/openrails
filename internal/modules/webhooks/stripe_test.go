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
      "subscription": "sub_test",
      "customer": "cus_test",
      "amount_paid": 1200,
      "currency": "usd",
      "metadata": {},
      "lines": {"data": [{"pricing": {"price_details": {"price": "price_stripe_initiate"}}}]},
      "parent": {"subscription_details": {"metadata": {
        "user_id": "019e51df-17aa-7de4-a4e9-4f2e7c33ea29",
        "internal_price_id": "4ecb4c2b-b057-49c0-a691-10639c0969db"
      }}}
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
