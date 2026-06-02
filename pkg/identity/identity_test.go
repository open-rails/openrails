package identity

import (
	"testing"

	"github.com/google/uuid"
)

// TestOwnerOrgIDFromString_ParsesUUID asserts a valid UUID subject resolves to
// the same owner org id (the caller-provided personal-org / account id).
func TestOwnerOrgIDFromString_ParsesUUID(t *testing.T) {
	u := uuid.New()
	got := OwnerOrgIDFromString(u.String())
	if got.UUID() != u {
		t.Fatalf("OwnerOrgIDFromString(%s) = %s, want %s", u, got, u)
	}
}

// TestOwnerOrgIDFromString_Empty returns the zero owner id for empty / blank
// input — callers must treat the zero value as "no owner resolved".
func TestOwnerOrgIDFromString_Empty(t *testing.T) {
	if !OwnerOrgIDFromString("").IsZero() {
		t.Fatal("empty input must yield the zero owner id")
	}
	if !OwnerOrgIDFromString("   ").IsZero() {
		t.Fatal("whitespace-only input must yield the zero owner id")
	}
}

// TestOwnerOrgIDFromString_NonUUID returns the zero owner id for a non-UUID
// subject — there is NO synthesized stand-in derivation.
func TestOwnerOrgIDFromString_NonUUID(t *testing.T) {
	if !OwnerOrgIDFromString("not-a-uuid").IsZero() {
		t.Fatal("non-UUID input must yield the zero owner id (no stand-in synthesis)")
	}
}
