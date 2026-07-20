package money

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

func TestInvoiceRetryIdempotencyKey(t *testing.T) {
	t.Parallel()
	invoiceID := uuid.New()
	key := invoiceRetryIdempotencyKey(invoiceID, "client-key")
	if len(key) > 50 {
		t.Fatalf("provider key length = %d, want at most 50", len(key))
	}
	if key != invoiceRetryIdempotencyKey(invoiceID, "client-key") {
		t.Fatal("provider key is not deterministic")
	}
	if key == invoiceRetryIdempotencyKey(uuid.New(), "client-key") {
		t.Fatal("provider key does not include invoice scope")
	}
	methodID := uuid.New()
	if attemptKey := invoiceRetryAttemptKey(key, methodID); attemptKey != key+":"+methodID.String() {
		t.Fatalf("attempt key = %q, want immutable method binding", attemptKey)
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
			name: "retry already in progress",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				LastCollectionFailureCode: stringPointer(collectionAttemptInProgress),
			},
		},
		{
			name: "provider outcome unknown",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				LastCollectionFailureCode: stringPointer(collectionOutcomeUnknown),
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

func TestIsCollectionOutcomeAmbiguous(t *testing.T) {
	t.Parallel()

	ambiguous := &nmi.TransportAmbiguousError{Err: errors.New("connection reset after send")}
	if !isCollectionOutcomeAmbiguous(ambiguous) {
		t.Fatal("transport-ambiguous provider error must be classified as ambiguous")
	}
	if isCollectionOutcomeAmbiguous(errors.New("provider unavailable before send")) {
		t.Fatal("clean provider failure must remain retryable")
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
			name: "attempt in progress",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				LastCollectionFailureCode: stringPointer(collectionAttemptInProgress),
			},
		},
		{
			name: "unknown outcome",
			invoice: &models.Invoice{
				Status: "past_due", CollectionMethod: CollectionChargeAutomatically, AmountDue: 100,
				LastCollectionFailureCode: stringPointer(collectionOutcomeUnknown),
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

func stringPointer(value string) *string {
	return &value
}
