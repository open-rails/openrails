package uuidutil

import (
	"testing"

	"github.com/google/uuid"
)

// ID-7: natural-key ids are a pure, injective function of (namespace, parts).
func TestDeterministicIDIsStableAndInjective(t *testing.T) {
	a := DeterministicID(DeterministicNamespace, "merchant-abc", "premium")
	if a != DeterministicID(DeterministicNamespace, "merchant-abc", "premium") || a.Version() != 5 || a == uuid.Nil {
		t.Fatalf("unstable or malformed id %s", a)
	}
	if a == DeterministicID(uuid.Nil, "merchant-abc", "premium") {
		t.Fatal("namespace must participate")
	}
	// Pinned: changing the namespace or encoding re-mints every persisted id.
	if got := DeterministicNamespace.String(); got != "6f2a1bc3-51cd-4daa-844f-99d170240561" {
		t.Fatalf("namespace changed to %s", got)
	}
	seen := map[uuid.UUID][]string{}
	for _, parts := range [][]string{
		{"a", "bc"}, {"ab", "c"}, {"abc"}, {"a", "b", "c"},
		{"a/b", "c"}, {"a", "b/c"}, {"", "abc"}, {"abc", ""}, {}, {""},
		{"945280-0000", "live"}, {"945280", "0000-live"},
	} {
		id := DeterministicID(DeterministicNamespace, parts...)
		if prev, ok := seen[id]; ok {
			t.Fatalf("collision: %q and %q", prev, parts)
		}
		seen[id] = parts
	}
	if NewV7().Version() != 7 || NewV7() == NewV7() {
		t.Fatal("NewV7 must mint distinct v7 ids")
	}
}
