package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestDefaultIDIsWellKnown(t *testing.T) {
	const want = "00000000-0000-0000-0000-000000000001"
	if got := DefaultID.String(); got != want {
		t.Fatalf("DefaultID = %s, want %s", got, want)
	}
	if !DefaultID.IsDefault() {
		t.Fatal("DefaultID.IsDefault() = false, want true")
	}
	if DefaultID.IsZero() {
		t.Fatal("DefaultID.IsZero() = true, want false")
	}
}

func TestFromContextOrDefault_DefaultsToDefaultTenant(t *testing.T) {
	// A bare context with no resolved tenant must default to the default tenant,
	// which is what lets single-tenant / self-hosted runs share the multi-tenant
	// code path.
	got := FromContextOrDefault(context.Background())
	if got != DefaultID {
		t.Fatalf("FromContextOrDefault(empty) = %s, want default %s", got, DefaultID)
	}

	// nil context must also be safe and default.
	//nolint:staticcheck // deliberately passing a nil context to assert safety
	if got := FromContextOrDefault(nil); got != DefaultID {
		t.Fatalf("FromContextOrDefault(nil) = %s, want default %s", got, DefaultID)
	}
}

func TestFromContextOrDefault_ReturnsResolvedTenant(t *testing.T) {
	other := ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	ctx := WithID(context.Background(), other)

	if got := FromContextOrDefault(ctx); got != other {
		t.Fatalf("FromContextOrDefault(resolved) = %s, want %s", got, other)
	}

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext(resolved) ok = false, want true")
	}
	if got != other {
		t.Fatalf("FromContext(resolved) = %s, want %s", got, other)
	}
}

func TestFromContext_NoTenant(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("FromContext(empty) ok = true, want false")
	}

	// A zero-valued tenant id stored on the context must be treated as unset.
	ctx := WithID(context.Background(), ID{})
	if _, ok := FromContext(ctx); ok {
		t.Fatal("FromContext(zero id) ok = true, want false")
	}
	if got := FromContextOrDefault(ctx); got != DefaultID {
		t.Fatalf("FromContextOrDefault(zero id) = %s, want default", got)
	}
}

func TestParseID(t *testing.T) {
	id, err := ParseID("11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("ParseID valid: unexpected error %v", err)
	}
	if id.String() != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("ParseID round-trip = %s", id.String())
	}

	if _, err := ParseID("not-a-uuid"); err == nil {
		t.Fatal("ParseID(invalid) err = nil, want error")
	}
}
