package money

import (
	"github.com/open-rails/openrails/internal/intents"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestInvoiceRetryOperationKey(t *testing.T) {
	t.Parallel()
	invoiceID := uuid.New()
	key := intents.InvoiceCollectionRetryKey(invoiceID, "client-key")
	if key != intents.InvoiceCollectionRetryKey(invoiceID, "client-key") {
		t.Fatal("operation key is not deterministic")
	}
	if key == intents.InvoiceCollectionRetryKey(uuid.New(), "client-key") {
		t.Fatal("operation key does not include invoice scope")
	}
	if key == intents.InvoiceCollectionRetryKey(invoiceID, "other-key") {
		t.Fatal("operation key does not include the client key")
	}
}

func TestInvoiceCollectionRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		invoice *models.Invoice
		want    bool
	}{
		{
			name: "past due automatic invoice",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
			},
			want: true,
		},
		{
			name: "uncollectible automatic invoice",
			invoice: &models.Invoice{
				Status: "uncollectible", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
			},
			want: true,
		},
		{
			name: "healthy open invoice",
			invoice: &models.Invoice{
				Status: "open", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
			},
		},
		{
			name: "manual remittance invoice",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionSendInvoice, AmountDue: 100,
			},
		},
		{
			name: "paid invoice",
			invoice: &models.Invoice{
				Status: "paid", CollectionMethod: CollectionChargeAutomatically,
			},
		},
		{
			name: "collection operation live",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				CollectionIntentID: uuidPointer(uuid.New()),
			},
		},
		{name: "nil invoice"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := invoiceCollectionRetryable(tt.invoice); got != tt.want {
				t.Fatalf("invoiceCollectionRetryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScheduledInvoiceCollectionEligible(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	tests := []struct {
		name    string
		invoice *models.Invoice
		want    bool
	}{
		{
			name: "new due invoice",
			invoice: &models.Invoice{
				Status: "open", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100, DueAt: &past,
			},
			want: true,
		},
		{
			name: "scheduled retry due",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				CollectionFailureCount: 1, NextCollectionAttemptAt: &past,
			},
			want: true,
		},
		{
			name: "collection operation live",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				CollectionFailureCount: 1, NextCollectionAttemptAt: &past, CollectionIntentID: uuidPointer(uuid.New()),
			},
		},
		{
			name: "future retry",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				CollectionFailureCount: 1, NextCollectionAttemptAt: &future,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := scheduledInvoiceCollectionEligible(tt.invoice, 0, now); got != tt.want {
				t.Fatalf("scheduledInvoiceCollectionEligible() = %v, want %v", got, tt.want)
			}
		})
	}
}

func uuidPointer(id uuid.UUID) *uuid.UUID {
	return &id
}
