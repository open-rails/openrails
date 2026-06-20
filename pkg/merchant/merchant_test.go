package merchant

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestWithIDAndFromContext(t *testing.T) {
	other := ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	ctx := WithID(context.Background(), other)

	got, ok := FromContext(ctx)
	if !ok {
		t.Fatal("FromContext(resolved) ok = false, want true")
	}
	if got != other {
		t.Fatalf("FromContext(resolved) = %s, want %s", got, other)
	}
}

func TestFromContext_NoMerchant(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("FromContext(empty) ok = true, want false")
	}

	// A zero-valued merchant id stored on the context must be treated as unset.
	ctx := WithID(context.Background(), ID{})
	if _, ok := FromContext(ctx); ok {
		t.Fatal("FromContext(zero id) ok = true, want false")
	}
}

func TestRequire(t *testing.T) {
	// Missing merchant surfaces as ErrNoMerchant, never a silent fallback.
	if _, err := Require(context.Background()); err != ErrNoMerchant {
		t.Fatalf("Require(empty) err = %v, want ErrNoMerchant", err)
	}

	want := ID(uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	got, err := Require(WithID(context.Background(), want))
	if err != nil {
		t.Fatalf("Require(resolved): unexpected error %v", err)
	}
	if got != want {
		t.Fatalf("Require(resolved) = %s, want %s", got, want)
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

func TestValidateSlug(t *testing.T) {
	// Must mirror AuthKit's org-slug rule (#548): lowercase a-z0-9 + hyphens,
	// no leading/trailing hyphen, <=63 chars; normalized (trim+lowercase) first.
	// Valid INCLUDING inputs that only become valid after normalize (trim+lower) —
	// "UPPER"->"upper", "  Tensorhub  "->"tensorhub" — exactly the canonical
	// lowercase form AuthKit org slugs require, so the invariant still holds.
	valid := []string{"tensorhub", "doujins", "monkey", "a", "ab", "a-b-c", "x1-2y", "  Tensorhub  ", "UPPER"}
	for _, s := range valid {
		if err := ValidateSlug(s); err != nil {
			t.Errorf("ValidateSlug(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"", "-x", "x-", "a_b", "a.b", "a b", "héllo", "a--b-"}
	// 64 chars exceeds the 63 cap.
	invalid = append(invalid, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	for _, s := range invalid {
		if err := ValidateSlug(s); err == nil {
			t.Errorf("ValidateSlug(%q) = nil, want error", s)
		}
	}
}
