package billingimport

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

func testResolver(fallback PSPRef, rows ...gen.OpenrailsPsp) *pspResolver {
	r := &pspResolver{
		byID:     map[uuid.UUID]gen.OpenrailsPsp{},
		byKey:    map[string]gen.OpenrailsPsp{},
		fallback: fallback,
	}
	for _, p := range rows {
		r.byID[p.ID] = p
		if p.Key != nil {
			r.byKey[pspKeyIndex(p.Rail, *p.Key)] = p
			r.known = append(r.known, p.Rail+"/"+*p.Key)
		}
	}
	return r
}

func psp(rail, key string) gen.OpenrailsPsp {
	return gen.OpenrailsPsp{ID: uuid.New(), Rail: rail, Key: &key}
}

// or#893: the nullable "unbound legacy lane" is gone. A declared row that names
// no PSP and no book default is a refusal, not an unattributed write.
func TestImportRefusesADeclaredRowWithNoPSP(t *testing.T) {
	mobius := psp("nmi", "mobius")
	r := testResolver(PSPRef{}, mobius)

	_, err := r.resolve(PSPRef{}, "nmi", "subscription legacy-1")
	if err == nil {
		t.Fatal("an unattributed declared row must refuse the import")
	}
	for _, want := range []string{"subscription legacy-1", "default_psp", "nmi/mobius"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q must name %q so the operator can fix the row", err, want)
		}
	}
}

func TestImportAttributesFromTheBookDefault(t *testing.T) {
	mobius := psp("nmi", "mobius")
	r := testResolver(PSPRef{Key: "mobius"}, mobius)

	got, err := r.resolve(PSPRef{}, "nmi", "subscription legacy-1")
	if err != nil || got != mobius.ID {
		t.Fatalf("book default = %v, %v; want %s", got, err, mobius.ID)
	}
}

// A per-row PSP wins over the book default — that is what makes a mixed book
// importable in one call.
func TestPerRowPSPOverridesTheBookDefault(t *testing.T) {
	mobius, paykings := psp("nmi", "mobius"), psp("nmi", "paykings")
	r := testResolver(PSPRef{Key: "mobius"}, mobius, paykings)

	got, err := r.resolve(PSPRef{Key: "paykings"}, "nmi", "subscription legacy-2")
	if err != nil || got != paykings.ID {
		t.Fatalf("per-row PSP = %v, %v; want %s", got, err, paykings.ID)
	}
	byID, err := r.resolve(PSPRef{ID: &paykings.ID}, "nmi", "subscription legacy-3")
	if err != nil || byID != paykings.ID {
		t.Fatalf("per-row PSP id = %v, %v; want %s", byID, err, paykings.ID)
	}
}

func TestImportRefusesAPSPThisMerchantDoesNotOwn(t *testing.T) {
	mobius := psp("nmi", "mobius")
	r := testResolver(PSPRef{}, mobius)

	foreign := uuid.New()
	if _, err := r.resolve(PSPRef{ID: &foreign}, "nmi", "subscription x"); err == nil ||
		!strings.Contains(err.Error(), "does not own") {
		t.Fatalf("foreign PSP id err = %v, want a not-owned refusal", err)
	}
	if _, err := r.resolve(PSPRef{Key: "nope"}, "nmi", "subscription x"); err == nil ||
		!strings.Contains(err.Error(), "does not own") {
		t.Fatalf("unknown PSP key err = %v, want a not-owned refusal", err)
	}
}

// A PSP is bound to one rail; naming a stripe account for an NMI row is a
// declaration error, not something to resolve past.
func TestImportRefusesAPSPOnADifferentRail(t *testing.T) {
	stripe := psp("stripe", "stripe")
	r := testResolver(PSPRef{}, stripe)

	if _, err := r.resolve(PSPRef{ID: &stripe.ID}, "nmi", "subscription x"); err == nil ||
		!strings.Contains(err.Error(), "rail") {
		t.Fatalf("cross-rail PSP err = %v, want a rail-mismatch refusal", err)
	}
}
