package uuidutil

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// The vectors are the derivation NotifyOverdueInvoices repeats in SQL.
func TestChildV7(t *testing.T) {
	for _, c := range []struct{ parent, want string }{
		{"019a0000-0000-7000-8000-000000000001", "019a0000-0000-7b90-ba50-270a2946f316"},
		{"6f2a1bc3-51cd-4daa-844f-99d170240561", "00000000-0000-76d6-bd57-e999a2798b7c"},
	} {
		got := ChildV7(uuid.MustParse(c.parent), "invoice_overdue")
		if got.String() != c.want || got.Version() != 7 || got.Variant() != uuid.RFC4122 {
			t.Fatalf("ChildV7(%s) = %s, want %s", c.parent, got, c.want)
		}
	}
}

func TestDerivedV7(t *testing.T) {
	at := time.Date(2026, 11, 24, 12, 0, 0, 123456789, time.UTC)
	id := DerivedV7(at, DeterministicNamespace, "key")
	if id != DerivedV7(at.Add(456789*time.Nanosecond), DeterministicNamespace, "key") {
		t.Fatal("an instant in the same millisecond changed the id")
	}
	if id == DerivedV7(at, DeterministicNamespace, "other") || id == DerivedV7(at, NewV7(), "key") {
		t.Fatal("another name or namespace kept the id")
	}
	if got := TimeOf(id); !got.Equal(at.Truncate(time.Millisecond)) {
		t.Fatalf("TimeOf = %s, want %s", got, at.Truncate(time.Millisecond))
	}
	if got := TimeOf(DerivedV7(time.Time{}, DeterministicNamespace, "key")); got.UnixMilli() != 0 {
		t.Fatalf("an instant before the epoch reads as %s", got)
	}
}
