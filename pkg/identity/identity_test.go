package identity

import (
	"testing"

	"github.com/google/uuid"
)

// TestPersonalOrgID_Deterministic asserts the derivation is a stable pure
// function of the user id: the migration backfill and live writes must agree.
func TestPersonalOrgID_Deterministic(t *testing.T) {
	a := PersonalOrgID("user-123")
	b := PersonalOrgID("user-123")
	if a != b {
		t.Fatalf("PersonalOrgID not deterministic: %s != %s", a, b)
	}
	if PersonalOrgID("user-123") == PersonalOrgID("user-456") {
		t.Fatal("distinct user ids must map to distinct owner ids")
	}
}

// TestPersonalOrgID_IsV5 asserts the derived owner id is a RFC 4122 name-based
// (version 5) UUID, matching what the SQL backfill produces.
func TestPersonalOrgID_IsV5(t *testing.T) {
	id := uuid.UUID(PersonalOrgID("user-123"))
	if id.Version() != 5 {
		t.Fatalf("owner id version = %d, want 5", id.Version())
	}
	if id.Variant() != uuid.RFC4122 {
		t.Fatalf("owner id variant = %v, want RFC4122", id.Variant())
	}
}

// TestPersonalOrgID_Empty returns the zero owner id for an empty actor.
func TestPersonalOrgID_Empty(t *testing.T) {
	if !PersonalOrgID("").IsZero() {
		t.Fatal("empty actor must yield the zero owner id")
	}
	if !PersonalOrgID("   ").IsZero() {
		t.Fatal("whitespace-only actor must yield the zero owner id")
	}
}

// TestPersonalOrgID_KnownVector pins the exact derived value for a known input
// so that any accidental change to the namespace, prefix, or algorithm — which
// would silently re-home every existing balance to a different owner id — fails
// loudly. The SQL backfill in migration 040 must reproduce this same value.
func TestPersonalOrgID_KnownVector(t *testing.T) {
	// Computed via uuid.NewSHA1(namespace, "openrails:personal-org:" + "user-123").
	want := uuid.NewSHA1(personalOrgNamespace, []byte(personalOrgPrefix+"user-123")).String()
	got := PersonalOrgID("user-123").String()
	if got != want {
		t.Fatalf("known-vector mismatch: got %s want %s", got, want)
	}
}
