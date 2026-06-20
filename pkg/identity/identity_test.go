package identity

import (
	"testing"

	"github.com/google/uuid"
)

// TestCustomerIDFromString_ParsesUUID asserts a valid UUID subject resolves to
// the same customer id (the caller-provided personal merchant-subject / account id).
func TestCustomerIDFromString_ParsesUUID(t *testing.T) {
	u := uuid.New()
	got := CustomerIDFromString(u.String())
	if got.UUID() != u {
		t.Fatalf("CustomerIDFromString(%s) = %s, want %s", u, got, u)
	}
}

// TestCustomerIDFromString_Empty returns the zero owner id for empty / blank
// input — callers must treat the zero value as "no owner resolved".
func TestCustomerIDFromString_Empty(t *testing.T) {
	if !CustomerIDFromString("").IsZero() {
		t.Fatal("empty input must yield the zero owner id")
	}
	if !CustomerIDFromString("   ").IsZero() {
		t.Fatal("whitespace-only input must yield the zero owner id")
	}
}

// TestCustomerIDFromString_NonUUID returns the zero owner id for a non-UUID
// subject — there is NO synthesized stand-in derivation.
func TestCustomerIDFromString_NonUUID(t *testing.T) {
	if !CustomerIDFromString("not-a-uuid").IsZero() {
		t.Fatal("non-UUID input must yield the zero owner id (no stand-in synthesis)")
	}
}
