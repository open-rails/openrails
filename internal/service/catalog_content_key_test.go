package service

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

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

	beforeSnap := catalog.BuildDriftSnapshot(beforeProducts, beforePrices, uuid.Nil)
	afterSnap := catalog.BuildDriftSnapshot(afterProducts, afterPrices, uuid.Nil)

	// The snapshot indexes prices by the content key — the same key must appear
	// in both snapshots even though every UUID changed.
	contentKey := openRailsPriceContentKey(productKey, currency, amount, cycle)
	if _, ok := beforeSnap.PriceByContentKey[contentKey]; !ok {
		t.Fatalf("before snapshot missing content key %q", contentKey)
	}
	if _, ok := afterSnap.PriceByContentKey[contentKey]; !ok {
		t.Fatalf("after snapshot missing content key %q", contentKey)
	}
	if _, ok := beforeSnap.ProductByKey[productKey]; !ok {
		t.Fatalf("before snapshot missing product key %q", productKey)
	}
	if _, ok := afterSnap.ProductByKey[productKey]; !ok {
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
	if events := catalog.ComputeStripeDrift(nil, stripePrices, beforeSnap, now); len(events) != 0 {
		t.Fatalf("pre-wipe: expected re-attach with no drift, got %+v", events)
	}
	if events := catalog.ComputeStripeDrift(nil, stripePrices, afterSnap, now); len(events) != 0 {
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
	snap := catalog.BuildDriftSnapshot(products, prices, uuid.Nil)

	// Stripe has a price at a DIFFERENT amount ($39.00) under its own (different)
	// content lookup_key. It must NOT match the $29 row — it is a separate price.
	stripePrices := []catalog.StripePrice{
		{ID: "price_39", UnitAmount: 3900, Currency: currency, Active: true, LookupKey: internalStripeLookupKey(productKey, currency, 39_000_000, cycle)},
	}
	now := time.Now().UTC()
	events := catalog.ComputeStripeDrift(nil, stripePrices, snap, now)
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

// fieldSet collapses a slice of drift events into a set of the fields that
// drifted, for terse assertions.
func fieldSet(events []models.CatalogDriftEvent) map[string]bool {
	out := make(map[string]bool, len(events))
	for _, e := range events {
		out[e.Field] = true
	}
	return out
}
