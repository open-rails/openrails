package identity

import (
	"testing"

	"github.com/google/uuid"
)

// TestTenantSubjectIDFromString_ParsesUUID asserts a valid UUID subject resolves to
// the same tenant subject id (the caller-provided personal tenant-subject / account id).
func TestTenantSubjectIDFromString_ParsesUUID(t *testing.T) {
	u := uuid.New()
	got := TenantSubjectIDFromString(u.String())
	if got.UUID() != u {
		t.Fatalf("TenantSubjectIDFromString(%s) = %s, want %s", u, got, u)
	}
}

// TestTenantSubjectIDFromString_Empty returns the zero owner id for empty / blank
// input — callers must treat the zero value as "no owner resolved".
func TestTenantSubjectIDFromString_Empty(t *testing.T) {
	if !TenantSubjectIDFromString("").IsZero() {
		t.Fatal("empty input must yield the zero owner id")
	}
	if !TenantSubjectIDFromString("   ").IsZero() {
		t.Fatal("whitespace-only input must yield the zero owner id")
	}
}

// TestTenantSubjectIDFromString_NonUUID returns the zero owner id for a non-UUID
// subject — there is NO synthesized stand-in derivation.
func TestTenantSubjectIDFromString_NonUUID(t *testing.T) {
	if !TenantSubjectIDFromString("not-a-uuid").IsZero() {
		t.Fatal("non-UUID input must yield the zero owner id (no stand-in synthesis)")
	}
}
