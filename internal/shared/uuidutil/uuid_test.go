package uuidutil

import (
	"testing"

	"github.com/google/uuid"
)

// TestDeterministicIDStable pins that a natural key always derives the same id
// (#662): the whole point is cross-process, cross-database reproducibility.
func TestDeterministicIDStable(t *testing.T) {
	ns := DeterministicNamespace
	got1 := DeterministicID(ns, "merchant-abc", "premium")
	got2 := DeterministicID(ns, "merchant-abc", "premium")
	if got1 != got2 {
		t.Fatalf("same parts must derive the same id: %s != %s", got1, got2)
	}
	// It is a uuidv5 (SHA-1 name-based).
	if got1.Version() != 5 {
		t.Fatalf("expected uuid version 5, got %d", got1.Version())
	}
	if got1 == uuid.Nil {
		t.Fatal("derived id must not be the nil uuid")
	}
}

// TestDeterministicIDInjective pins that the length-prefixed encoding is
// unambiguous across field boundaries: no separator collision, and the split
// point between parts is itself part of the identity.
func TestDeterministicIDInjective(t *testing.T) {
	ns := DeterministicNamespace
	cases := [][]string{
		{"a", "bc"},
		{"ab", "c"}, // same concatenation as {"a","bc"}, different boundary
		{"abc"},     // same bytes, different arity
		{"a", "b", "c"},
		{"a/b", "c"}, // a part containing the kind of separator naive joins use
		{"a", "b/c"},
		{"", "abc"},             // empty leading part
		{"abc", ""},             // empty trailing part
		{"945280-0000", "live"}, // CCBill-style account_id (contains '-')
		{"945280", "0000-live"},
	}
	seen := map[uuid.UUID][]string{}
	for _, parts := range cases {
		id := DeterministicID(ns, parts...)
		if prev, ok := seen[id]; ok {
			t.Fatalf("collision: %v and %v derived the same id %s", prev, parts, id)
		}
		seen[id] = parts
	}
}

// TestDeterministicIDNamespaceScoped pins that the namespace participates in
// the derivation — a different namespace yields a different id for the same
// parts (so the permanent DeterministicNamespace constant genuinely anchors
// every derived id).
func TestDeterministicIDNamespaceScoped(t *testing.T) {
	parts := []string{"merchant-abc", "premium"}
	a := DeterministicID(DeterministicNamespace, parts...)
	b := DeterministicID(uuid.MustParse("00000000-0000-0000-0000-000000000000"), parts...)
	if a == b {
		t.Fatal("different namespaces must derive different ids for the same parts")
	}
}
