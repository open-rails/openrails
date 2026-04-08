package webhooks

import (
	"encoding/json"
	"testing"
	"time"

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
