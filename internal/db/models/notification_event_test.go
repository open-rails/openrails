package models

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
)

// A fact recorded before invoices were named reads as one recorded now.
func TestLegacyNotification(t *testing.T) {
	id := uuid.MustParse("0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b")
	for stored, want := range map[string]NotificationEventType{
		"order_due": NotificationInvoiceDue, "order_overdue": NotificationInvoiceOverdue,
		"order_collection_stopped": NotificationInvoiceCollectionStopped, "invoice_due": NotificationInvoiceDue, "premium_ended": NotificationPremiumEnded,
	} {
		if got := NotificationType(stored); got != want {
			t.Errorf("NotificationType(%q) = %q; want %q", stored, got, want)
		}
	}
	stored := StoredNotificationTypes(NotificationInvoiceCollectionStopped, NotificationPremiumEnded)
	if len(stored) != 3 || stored[0] != "invoice_collection_stopped" || stored[1] != "premium_ended" || stored[2] != "order_collection_stopped" {
		t.Errorf("StoredNotificationTypes = %v", stored)
	}
	for _, raw := range []string{
		`{"order_id":"ord_` + id.String() + `","order_number":1001,"overdue_orders":2,"currency":"USD"}`,
		`{"invoice_id":"in_` + id.String() + `","invoice_number":1001,"overdue_invoices":2,"currency":"USD"}`,
	} {
		var d billing.NotificationData
		if err := DecodeNotificationData([]byte(raw), &d); err != nil {
			t.Fatalf("DecodeNotificationData(%s): %v", raw, err)
		}
		if d.InvoiceID.UUID() != id || d.InvoiceNumber != 1001 || d.OverdueInvoices != 2 || d.Currency != "USD" {
			t.Errorf("DecodeNotificationData(%s) = %+v", raw, d)
		}
	}
}
