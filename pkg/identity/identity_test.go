package identity

import (
	"testing"

	"github.com/google/uuid"
)

// TestMerchantSubjectIDFromString_ParsesUUID asserts a valid UUID subject resolves to
// the same tenant subject id (the caller-provided personal tenant-subject / account id).
func TestMerchantSubjectIDFromString_ParsesUUID(t *testing.T) {
	u := uuid.New()
	got := MerchantSubjectIDFromString(u.String())
	if got.UUID() != u {
		t.Fatalf("MerchantSubjectIDFromString(%s) = %s, want %s", u, got, u)
	}
}

// TestMerchantSubjectIDFromString_Empty returns the zero owner id for empty / blank
// input — callers must treat the zero value as "no owner resolved".
func TestMerchantSubjectIDFromString_Empty(t *testing.T) {
	if !MerchantSubjectIDFromString("").IsZero() {
		t.Fatal("empty input must yield the zero owner id")
	}
	if !MerchantSubjectIDFromString("   ").IsZero() {
		t.Fatal("whitespace-only input must yield the zero owner id")
	}
}

// TestMerchantSubjectIDFromString_NonUUID returns the zero owner id for a non-UUID
// subject — there is NO synthesized stand-in derivation.
func TestMerchantSubjectIDFromString_NonUUID(t *testing.T) {
	if !MerchantSubjectIDFromString("not-a-uuid").IsZero() {
		t.Fatal("non-UUID input must yield the zero owner id (no stand-in synthesis)")
	}
}
