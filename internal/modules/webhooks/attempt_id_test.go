package webhooks

import (
	"testing"

	"github.com/google/uuid"
)

// A provider echoes the attempt id it was given: iat_ now, oat_ on a sale or
// Checkout Session opened before attempts were invoice attempts.
func TestProviderAttemptID(t *testing.T) {
	id := uuid.MustParse("0193a1b2-c3d4-7e5f-8a6b-7c8d9e0f1a2b")
	for _, raw := range []string{"iat_" + id.String(), "oat_" + id.String(), " oat_" + id.String() + " "} {
		if got, ok := ccbillReservation(raw); !ok || got != id {
			t.Errorf("ccbillReservation(%q) = %v, %v; want %v", raw, got, ok, id)
		}
		if got := parseInvoiceAttemptID(map[string]string{"order_attempt_id": raw}); got != id {
			t.Errorf("parseInvoiceAttemptID(%q) = %v; want %v", raw, got, id)
		}
	}
	for _, raw := range []string{"", id.String(), "in_" + id.String(), "oat_", "oat_" + uuid.Nil.String(), "iat_not-a-uuid"} {
		if got, ok := ccbillReservation(raw); ok {
			t.Errorf("ccbillReservation(%q) = %v; want refused", raw, got)
		}
		if got := parseInvoiceAttemptID(map[string]string{"order_attempt_id": raw}); got != uuid.Nil {
			t.Errorf("parseInvoiceAttemptID(%q) = %v; want nil", raw, got)
		}
	}
	if got := parseInvoiceAttemptID(nil); got != uuid.Nil {
		t.Errorf("parseInvoiceAttemptID(nil) = %v; want nil", got)
	}
}
