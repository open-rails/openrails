package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// TestContentKeysAreSlugDeterministic asserts that the Stripe content keys are
// pure functions of (product_slug, price_slug) and never incorporate a row
// UUID. This is the invariant that makes wipe+resync re-attach to existing
// provider objects instead of duplicating them: regenerating the OpenRails row
// UUIDs must not change the lookup_key or the price content key.
func TestContentKeysAreSlugDeterministic(t *testing.T) {
	const (
		productSlug = "premium"
		priceSlug   = "monthly"
	)

	wantLookup := "openrails.premium.monthly"
	wantContent := "premium.monthly"

	// Exact format assertions.
	if got := internalStripeLookupKey(productSlug, priceSlug); got != wantLookup {
		t.Fatalf("internalStripeLookupKey = %q, want %q", got, wantLookup)
	}
	if got := openRailsPriceContentKey(productSlug, priceSlug); got != wantContent {
		t.Fatalf("openRailsPriceContentKey = %q, want %q", got, wantContent)
	}

	// Determinism: the same slugs must always produce the same keys, regardless
	// of how many times we call (no hidden UUID / time / counter input).
	for i := 0; i < 5; i++ {
		if got := internalStripeLookupKey(productSlug, priceSlug); got != wantLookup {
			t.Fatalf("call %d: lookup not stable: %q != %q", i, got, wantLookup)
		}
		if got := openRailsPriceContentKey(productSlug, priceSlug); got != wantContent {
			t.Fatalf("call %d: content key not stable: %q != %q", i, got, wantContent)
		}
	}

	// Whitespace around slugs must not change the key (trimmed identity).
	if internalStripeLookupKey("  premium ", " monthly  ") != wantLookup {
		t.Fatal("lookup key must trim slug whitespace")
	}
	if openRailsPriceContentKey(" premium  ", "  monthly ") != wantContent {
		t.Fatal("content key must trim slug whitespace")
	}

	// Different slugs must produce different keys (no collisions for distinct
	// content identities).
	if internalStripeLookupKey("premium", "monthly") == internalStripeLookupKey("premium", "yearly") {
		t.Fatal("distinct price slugs must yield distinct lookup keys")
	}
	if internalStripeLookupKey("premium", "monthly") == internalStripeLookupKey("basic", "monthly") {
		t.Fatal("distinct product slugs must yield distinct lookup keys")
	}
}

// TestContentKeysSurviveUUIDRegeneration is the wipe-resync invariant stated
// directly: take two price rows that share the same (product_slug, price_slug)
// but have completely different row UUIDs — as would happen after a DB wipe and
// reseed — and assert their derived content keys are byte-identical. Because
// reconciliation reverse-matches Stripe objects to OpenRails rows by these
// content keys, identical keys mean the re-seeded rows re-attach to the existing
// Stripe objects rather than producing orphan/duplicate drift.
func TestContentKeysSurviveUUIDRegeneration(t *testing.T) {
	const (
		productSlug = "pro"
		priceSlug   = "annual"
	)

	// "Before wipe" identifiers.
	beforeProductID := uuid.New()
	beforePriceID := uuid.New()
	// "After wipe" identifiers — fresh UUIDs, same slugs.
	afterProductID := uuid.New()
	afterPriceID := uuid.New()

	if beforeProductID == afterProductID || beforePriceID == afterPriceID {
		t.Fatal("precondition: regenerated UUIDs should differ")
	}

	beforeProducts := []*models.Product{{ID: beforeProductID, Slug: productSlug, Status: models.CatalogStatusActive}}
	beforePrices := []*models.Price{{ID: beforePriceID, ProductID: beforeProductID, Slug: priceSlug, Amount: 1000, Currency: "usd", Status: models.CatalogStatusActive}}

	afterProducts := []*models.Product{{ID: afterProductID, Slug: productSlug, Status: models.CatalogStatusActive}}
	afterPrices := []*models.Price{{ID: afterPriceID, ProductID: afterProductID, Slug: priceSlug, Amount: 1000, Currency: "usd", Status: models.CatalogStatusActive}}

	beforeSnap := buildSnapshotFromRows(beforeProducts, beforePrices)
	afterSnap := buildSnapshotFromRows(afterProducts, afterPrices)

	// The snapshot indexes prices by the content key — the same key must appear
	// in both snapshots even though every UUID changed.
	contentKey := openRailsPriceContentKey(productSlug, priceSlug)
	if _, ok := beforeSnap.priceByContentKey[contentKey]; !ok {
		t.Fatalf("before snapshot missing content key %q", contentKey)
	}
	if _, ok := afterSnap.priceByContentKey[contentKey]; !ok {
		t.Fatalf("after snapshot missing content key %q", contentKey)
	}
	if _, ok := beforeSnap.productBySlug[productSlug]; !ok {
		t.Fatalf("before snapshot missing product slug %q", productSlug)
	}
	if _, ok := afterSnap.productBySlug[productSlug]; !ok {
		t.Fatalf("after snapshot missing product slug %q", productSlug)
	}

	// A single Stripe price with the slug-derived lookup_key must match BOTH the
	// pre-wipe and post-wipe rows with zero drift (i.e. re-attach, never
	// duplicate). If the keys depended on UUIDs, the post-wipe pass would emit
	// an orphan_in_stripe instead.
	stripePrices := []catalog.StripePrice{
		{ID: "price_live", UnitAmount: 1000, Currency: "usd", Active: true, LookupKey: internalStripeLookupKey(productSlug, priceSlug)},
	}
	now := time.Now().UTC()
	if events := computeCatalogDrift(nil, stripePrices, beforeSnap, now); len(events) != 0 {
		t.Fatalf("pre-wipe: expected re-attach with no drift, got %+v", events)
	}
	if events := computeCatalogDrift(nil, stripePrices, afterSnap, now); len(events) != 0 {
		t.Fatalf("post-wipe: expected re-attach with no drift, got %+v", events)
	}
}

// TestDiffPriceFieldsAmountDrift asserts diffPriceFields flags unit_amount and
// currency divergence between the local OpenRails price and the Stripe price.
func TestDiffPriceFieldsAmountDrift(t *testing.T) {
	now := time.Now().UTC()
	local := &models.Price{ID: uuid.New(), Amount: 1000, Currency: "usd", Status: models.CatalogStatusActive}

	// Amount diverges only.
	sp := catalog.StripePrice{ID: "price_1", UnitAmount: 2000, Currency: "usd", Active: true}
	events := diffPriceFields(local, sp, now)
	if got := fieldSet(events); !got["unit_amount"] {
		t.Fatalf("expected unit_amount drift, got fields %v", got)
	} else if got["currency"] || got["active"] {
		t.Fatalf("expected only unit_amount drift, got fields %v", got)
	}

	// Currency diverges only.
	sp = catalog.StripePrice{ID: "price_1", UnitAmount: 1000, Currency: "eur", Active: true}
	events = diffPriceFields(local, sp, now)
	if got := fieldSet(events); !got["currency"] {
		t.Fatalf("expected currency drift, got fields %v", got)
	} else if got["unit_amount"] || got["active"] {
		t.Fatalf("expected only currency drift, got fields %v", got)
	}

	// Both diverge.
	sp = catalog.StripePrice{ID: "price_1", UnitAmount: 2500, Currency: "gbp", Active: true}
	events = diffPriceFields(local, sp, now)
	got := fieldSet(events)
	if !got["unit_amount"] || !got["currency"] {
		t.Fatalf("expected both unit_amount and currency drift, got fields %v", got)
	}

	// Fully in sync (currency compared case-insensitively) -> no drift.
	sp = catalog.StripePrice{ID: "price_1", UnitAmount: 1000, Currency: "USD", Active: true}
	if events := diffPriceFields(local, sp, now); len(events) != 0 {
		t.Fatalf("expected no drift when in sync, got %+v", events)
	}
}

// fieldSet collapses a slice of drift events into a set of the fields that
// drifted, for terse assertions.
func fieldSet(events []models.CatalogDriftEvent) map[string]bool {
	out := make(map[string]bool, len(events))
	for _, e := range events {
		out[e.Field] = true
	}
	return out
}

// TestHasImmutablePriceDrift is the reconcile decision predicate: an immutable
// price-term drift (unit_amount / currency) must take the re-mint path (new
// price + transfer_lookup_key + archive old), whereas mutable-only drift
// (is_active) takes the plain Update path.
//
// This unit-tests the decision predicate directly. The full re-mint round-trip
// (CreatePrice + UpdatePrice archive against Stripe + the DB UpdateProcessors
// write) is exercised by the DB/Stripe integration harness, because
// ReconcilePrice instantiates a concrete catalog.StripeCatalogService inline
// rather than through an injectable interface — there is no in-process fake to
// assert the create+archive calls against.
func TestHasImmutablePriceDrift(t *testing.T) {
	cases := []struct {
		name  string
		drift []DriftField
		want  bool
	}{
		{"no drift", nil, false},
		{"unit_amount drift -> remint", []DriftField{{Field: "unit_amount"}}, true},
		{"currency drift -> remint", []DriftField{{Field: "currency"}}, true},
		{"is_active only -> plain update", []DriftField{{Field: "is_active"}}, false},
		{"is_active + unit_amount -> remint", []DriftField{{Field: "is_active"}, {Field: "unit_amount"}}, true},
		{"unknown field only -> plain update", []DriftField{{Field: "nickname"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasImmutablePriceDrift(tc.drift); got != tc.want {
				t.Fatalf("hasImmutablePriceDrift(%+v) = %v, want %v", tc.drift, got, tc.want)
			}
		})
	}
}
