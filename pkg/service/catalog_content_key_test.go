package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// TestContentKeysAreFinancialDeterministic asserts that the Stripe content keys
// are pure functions of (product_key, currency, unit_amount, billing_cycle)
// and never incorporate a row UUID. This is the invariant that makes wipe+resync
// re-attach to existing provider objects instead of duplicating them:
// regenerating the OpenRails row UUIDs must not change the lookup_key or the
// price content key.
func TestContentKeysAreFinancialDeterministic(t *testing.T) {
	const (
		productKey = "pro"
		currency   = "usd"
		amount     = int64(29_000_000)
	)
	cycle := intPtr(30)

	wantLookup := "openrails.pro.usd.29000000.30"
	wantContent := "pro.usd.29000000.30"

	// Exact format assertions.
	if got := internalStripeLookupKey(productKey, currency, amount, cycle); got != wantLookup {
		t.Fatalf("internalStripeLookupKey = %q, want %q", got, wantLookup)
	}
	if got := openRailsPriceContentKey(productKey, currency, amount, cycle); got != wantContent {
		t.Fatalf("openRailsPriceContentKey = %q, want %q", got, wantContent)
	}

	// One-time price (nil cycle and 0 cycle) renders the "onetime" token.
	wantOneTime := "openrails.pro.usd.5000000.onetime"
	if got := internalStripeLookupKey(productKey, currency, 5_000_000, nil); got != wantOneTime {
		t.Fatalf("one-time (nil) lookup = %q, want %q", got, wantOneTime)
	}
	if got := internalStripeLookupKey(productKey, currency, 5_000_000, intPtr(0)); got != wantOneTime {
		t.Fatalf("one-time (0) lookup = %q, want %q", got, wantOneTime)
	}

	// Determinism: same inputs always produce the same keys (no hidden UUID /
	// time / counter input).
	for i := 0; i < 5; i++ {
		if got := internalStripeLookupKey(productKey, currency, amount, cycle); got != wantLookup {
			t.Fatalf("call %d: lookup not stable: %q != %q", i, got, wantLookup)
		}
	}

	// Whitespace + currency case must not change the key (trimmed/lowercased).
	if internalStripeLookupKey("  pro ", " USD ", amount, cycle) != wantLookup {
		t.Fatal("lookup key must trim/lowercase its inputs")
	}

	// A DIFFERENT AMOUNT must yield a DIFFERENT key — this is the core of the
	// model: a price change is a new price (new content key), never a mutation.
	if internalStripeLookupKey(productKey, currency, 29_000_000, cycle) == internalStripeLookupKey(productKey, currency, 39_000_000, cycle) {
		t.Fatal("distinct amounts must yield distinct lookup keys (a price change is a new price)")
	}
	// Distinct currency / cycle / product also yield distinct keys.
	if internalStripeLookupKey(productKey, "usd", amount, cycle) == internalStripeLookupKey(productKey, "eur", amount, cycle) {
		t.Fatal("distinct currencies must yield distinct lookup keys")
	}
	if internalStripeLookupKey(productKey, currency, amount, intPtr(30)) == internalStripeLookupKey(productKey, currency, amount, intPtr(365)) {
		t.Fatal("distinct billing cycles must yield distinct lookup keys")
	}
	if internalStripeLookupKey("pro", currency, amount, cycle) == internalStripeLookupKey("basic", currency, amount, cycle) {
		t.Fatal("distinct product keys must yield distinct lookup keys")
	}
}

// TestContentKeysSurviveUUIDRegeneration is the wipe-resync invariant stated
// directly: take two price rows that share the same (product_key, price_terms)
// but have completely different row UUIDs — as would happen after a DB wipe and
// reseed — and assert their derived content keys are byte-identical. Because
// reconciliation reverse-matches Stripe objects to OpenRails rows by these
// content keys, identical keys mean the re-seeded rows re-attach to the existing
// Stripe objects rather than producing orphan/duplicate drift.
func TestContentKeysSurviveUUIDRegeneration(t *testing.T) {
	const (
		productKey  = "pro"
		currency    = "usd"
		amount      = int64(10_000_000)
		amountCents = int64(1000)
	)
	cycle := intPtr(365)
	accessHours := intPtr(365 * 24)

	// "Before wipe" identifiers.
	beforeProductID := uuid.New()
	beforePriceID := uuid.New()
	// "After wipe" identifiers — fresh UUIDs, same product key + money terms.
	afterProductID := uuid.New()
	afterPriceID := uuid.New()

	if beforeProductID == afterProductID || beforePriceID == afterPriceID {
		t.Fatal("precondition: regenerated UUIDs should differ")
	}

	beforeProducts := []*models.Product{{ID: beforeProductID, Key: productKey}}
	beforePrices := []*models.Price{{ID: beforePriceID, ProductID: beforeProductID, Amount: amount, Currency: currency, AccessDurationHours: accessHours, AutoRenew: true}}

	afterProducts := []*models.Product{{ID: afterProductID, Key: productKey}}
	afterPrices := []*models.Price{{ID: afterPriceID, ProductID: afterProductID, Amount: amount, Currency: currency, AccessDurationHours: accessHours, AutoRenew: true}}

	beforeSnap := buildSnapshotFromRows(beforeProducts, beforePrices)
	afterSnap := buildSnapshotFromRows(afterProducts, afterPrices)

	// The snapshot indexes prices by the content key — the same key must appear
	// in both snapshots even though every UUID changed.
	contentKey := openRailsPriceContentKey(productKey, currency, amount, cycle)
	if _, ok := beforeSnap.priceByContentKey[contentKey]; !ok {
		t.Fatalf("before snapshot missing content key %q", contentKey)
	}
	if _, ok := afterSnap.priceByContentKey[contentKey]; !ok {
		t.Fatalf("after snapshot missing content key %q", contentKey)
	}
	if _, ok := beforeSnap.productByKey[productKey]; !ok {
		t.Fatalf("before snapshot missing product key %q", productKey)
	}
	if _, ok := afterSnap.productByKey[productKey]; !ok {
		t.Fatalf("after snapshot missing product key %q", productKey)
	}

	// A single Stripe price with the content-derived lookup_key must match BOTH
	// the pre-wipe and post-wipe rows with zero drift (i.e. re-attach, never
	// duplicate). If the keys depended on UUIDs, the post-wipe pass would emit
	// an orphan_in_stripe instead.
	stripePrices := []catalog.StripePrice{
		{ID: "price_live", UnitAmount: amountCents, Currency: currency, Active: true, LookupKey: internalStripeLookupKey(productKey, currency, amount, cycle)},
	}
	now := time.Now().UTC()
	if events := computeCatalogDrift(nil, stripePrices, beforeSnap, now); len(events) != 0 {
		t.Fatalf("pre-wipe: expected re-attach with no drift, got %+v", events)
	}
	if events := computeCatalogDrift(nil, stripePrices, afterSnap, now); len(events) != 0 {
		t.Fatalf("post-wipe: expected re-attach with no drift, got %+v", events)
	}
}

// TestDifferentAmountIsADifferentPrice proves the core of the new model: because
// the amount is baked into the content key, a price at a different amount has a
// DIFFERENT content key and therefore does not reverse-match the old row — it
// surfaces as orphan_in_stripe, i.e. a brand-new price. There is no amount-drift
// "mutate in place / transfer lookup_key" path: a price change is create-new.
func TestDifferentAmountIsADifferentPrice(t *testing.T) {
	const (
		productKey = "pro"
		currency   = "usd"
	)
	cycle := intPtr(30)
	accessHours := intPtr(30 * 24)
	productID := uuid.New()
	priceID := uuid.New()

	// Local catalog has the $29.00 price.
	products := []*models.Product{{ID: productID, Key: productKey}}
	prices := []*models.Price{{ID: priceID, ProductID: productID, Amount: 29_000_000, Currency: currency, AccessDurationHours: accessHours, AutoRenew: true}}
	snap := buildSnapshotFromRows(products, prices)

	// Stripe has a price at a DIFFERENT amount ($39.00) under its own (different)
	// content lookup_key. It must NOT match the $29 row — it is a separate price.
	stripePrices := []catalog.StripePrice{
		{ID: "price_39", UnitAmount: 3900, Currency: currency, Active: true, LookupKey: internalStripeLookupKey(productKey, currency, 39_000_000, cycle)},
	}
	now := time.Now().UTC()
	events := computeCatalogDrift(nil, stripePrices, snap, now)
	if got := fieldSet(events); got["unit_amount"] {
		t.Fatalf("a different amount must NOT be reported as amount drift on the old price: %+v", events)
	}
	orphans := 0
	for _, e := range events {
		if e.Kind == models.CatalogDriftOrphanInStripe {
			orphans++
		}
	}
	if orphans != 1 {
		t.Fatalf("expected the $39 price to surface as 1 orphan_in_stripe (a new price), got %d (events=%+v)", orphans, events)
	}
}

// TestDiffPriceFieldsAmountDrift asserts diffPriceFields flags unit_amount and
// currency divergence between the local OpenRails price and the Stripe price.
func TestDiffPriceFieldsAmountDrift(t *testing.T) {
	now := time.Now().UTC()
	local := &models.Price{ID: uuid.New(), Amount: 10_000_000, Currency: "usd"}

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
