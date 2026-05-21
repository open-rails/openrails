package service

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
)

// fakeStripeLister serves fixture pages for the catalog reconciliation diff.
// It exercises the starting_after pagination loop in fetchStripeCatalog.
type fakeStripeLister struct {
	productPages [][]catalog.StripeProduct
	pricePages   [][]catalog.StripePrice
	productCalls int
	priceCalls   int
}

func (f *fakeStripeLister) ListProducts(_ context.Context, startingAfter string) ([]catalog.StripeProduct, string, error) {
	idx := f.productCalls
	f.productCalls++
	if idx >= len(f.productPages) {
		return nil, "", nil
	}
	page := f.productPages[idx]
	next := ""
	if idx+1 < len(f.productPages) && len(page) > 0 {
		next = page[len(page)-1].ID
	}
	return page, next, nil
}

func (f *fakeStripeLister) ListPrices(_ context.Context, startingAfter string) ([]catalog.StripePrice, string, error) {
	idx := f.priceCalls
	f.priceCalls++
	if idx >= len(f.pricePages) {
		return nil, "", nil
	}
	page := f.pricePages[idx]
	next := ""
	if idx+1 < len(f.pricePages) && len(page) > 0 {
		next = page[len(page)-1].ID
	}
	return page, next, nil
}

func TestFetchStripeCatalogPaginates(t *testing.T) {
	lister := &fakeStripeLister{
		productPages: [][]catalog.StripeProduct{
			{{ID: "prod_1"}, {ID: "prod_2"}},
			{{ID: "prod_3"}},
		},
		pricePages: [][]catalog.StripePrice{
			{{ID: "price_1"}},
			{{ID: "price_2"}, {ID: "price_3"}},
		},
	}
	products, prices, err := fetchStripeCatalog(context.Background(), lister)
	if err != nil {
		t.Fatalf("fetchStripeCatalog: %v", err)
	}
	if len(products) != 3 {
		t.Fatalf("expected 3 products across pages, got %d", len(products))
	}
	if len(prices) != 3 {
		t.Fatalf("expected 3 prices across pages, got %d", len(prices))
	}
	// Two non-empty product pages -> 2 calls (second page returns next="" so loop ends).
	if lister.productCalls != 2 {
		t.Fatalf("expected 2 product list calls, got %d", lister.productCalls)
	}
	if lister.priceCalls != 2 {
		t.Fatalf("expected 2 price list calls, got %d", lister.priceCalls)
	}
}

// helper builders
func prod(id uuid.UUID, name, desc string, active bool) *models.Product {
	return &models.Product{ID: id, DisplayName: name, Description: desc, IsActive: active}
}

func price(id, productID uuid.UUID, name string, amount int64, currency string, active bool, stripePriceID, stripeProductID string) *models.Price {
	p := &models.Price{ID: id, ProductID: productID, DisplayName: name, Amount: amount, Currency: currency, IsActive: active}
	if stripePriceID != "" || stripeProductID != "" {
		p.Processors = map[string]map[string]string{"stripe": {}}
		if stripePriceID != "" {
			p.Processors["stripe"][models.ProcessorKeyStripePriceID] = stripePriceID
		}
		if stripeProductID != "" {
			p.Processors["stripe"][models.ProcessorKeyStripeProductID] = stripeProductID
		}
	}
	return p
}

func countKind(events []models.CatalogDriftEvent, kind models.CatalogDriftKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestComputeCatalogDriftOrphan(t *testing.T) {
	now := time.Now().UTC()
	// Stripe has a product with no openrails marker -> orphan.
	stripeProducts := []catalog.StripeProduct{
		{ID: "prod_native", Name: "Native", Active: true, Metadata: map[string]string{}},
	}
	// Stripe price with marker pointing to a nonexistent OpenRails price -> orphan.
	stripePrices := []catalog.StripePrice{
		{ID: "price_ghost", UnitAmount: 1000, Currency: "usd", Active: true, Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsPriceID: uuid.New().String(),
		}},
	}
	snap := localCatalogSnapshot{
		productByID:      map[string]*models.Product{},
		priceByID:        map[string]*models.Price{},
		stripeProductIDs: map[string]string{},
		stripePriceIDs:   map[string]string{},
	}
	events := computeCatalogDrift(stripeProducts, stripePrices, snap, now)
	if got := countKind(events, models.CatalogDriftOrphanInStripe); got != 2 {
		t.Fatalf("expected 2 orphan events, got %d (events=%+v)", got, events)
	}
}

func TestComputeCatalogDriftMissingInStripe(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	// OpenRails believes it owns stripe price/product ids that are NOT in the pull.
	snap := localCatalogSnapshot{
		productByID:      map[string]*models.Product{productID.String(): prod(productID, "P", "", true)},
		priceByID:        map[string]*models.Price{priceID.String(): price(priceID, productID, "Monthly", 1000, "usd", true, "price_gone", "prod_gone")},
		stripeProductIDs: map[string]string{"prod_gone": productID.String()},
		stripePriceIDs:   map[string]string{"price_gone": priceID.String()},
	}
	// Stripe returns nothing.
	events := computeCatalogDrift(nil, nil, snap, now)
	if got := countKind(events, models.CatalogDriftMissingInStripe); got != 2 {
		t.Fatalf("expected 2 missing_in_stripe events, got %d (events=%+v)", got, events)
	}
}

func TestComputeCatalogDriftFieldDrift(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	localProduct := prod(productID, "Premium", "old desc", true)
	localPrice := price(priceID, productID, "Monthly", 1000, "usd", true, "price_1", "prod_1")

	stripeProducts := []catalog.StripeProduct{
		{ID: "prod_1", Name: "Premium Plus", Description: "new desc", Active: false, Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsProductID: productID.String(),
		}},
	}
	stripePrices := []catalog.StripePrice{
		{ID: "price_1", UnitAmount: 2000, Currency: "eur", Active: false, Nickname: "Yearly", Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsPriceID: priceID.String(),
		}},
	}
	snap := localCatalogSnapshot{
		productByID:      map[string]*models.Product{productID.String(): localProduct},
		priceByID:        map[string]*models.Price{priceID.String(): localPrice},
		stripeProductIDs: map[string]string{"prod_1": productID.String()},
		stripePriceIDs:   map[string]string{"price_1": priceID.String()},
	}
	events := computeCatalogDrift(stripeProducts, stripePrices, snap, now)
	driftCount := countKind(events, models.CatalogDriftFieldDrift)
	// product: name, description, active = 3. price: unit_amount, currency, active, nickname = 4.
	if driftCount != 7 {
		t.Fatalf("expected 7 field_drift events, got %d (events=%+v)", driftCount, events)
	}
	if countKind(events, models.CatalogDriftMissingInStripe) != 0 {
		t.Fatalf("expected no missing events when stripe ids are present")
	}
}

func TestComputeCatalogDriftNoDriftWhenInSync(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	localProduct := prod(productID, "Premium", "desc", true)
	localPrice := price(priceID, productID, "Monthly", 1000, "usd", true, "price_1", "prod_1")

	stripeProducts := []catalog.StripeProduct{
		{ID: "prod_1", Name: "Premium", Description: "desc", Active: true, Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsProductID: productID.String(),
		}},
	}
	stripePrices := []catalog.StripePrice{
		{ID: "price_1", UnitAmount: 1000, Currency: "usd", Active: true, Nickname: "Monthly", Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsPriceID: priceID.String(),
		}},
	}
	snap := localCatalogSnapshot{
		productByID:      map[string]*models.Product{productID.String(): localProduct},
		priceByID:        map[string]*models.Price{priceID.String(): localPrice},
		stripeProductIDs: map[string]string{"prod_1": productID.String()},
		stripePriceIDs:   map[string]string{"price_1": priceID.String()},
	}
	events := computeCatalogDrift(stripeProducts, stripePrices, snap, now)
	if len(events) != 0 {
		t.Fatalf("expected zero drift events when fully in sync, got %d: %+v", len(events), events)
	}
}

// TestDriftDedupeKeyStable asserts the dedupe key is identical across reruns for
// the same logical divergence, which is what makes persistence idempotent.
func TestDriftDedupeKeyStable(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	mk := func() models.CatalogDriftEvent {
		return models.CatalogDriftEvent{
			Kind:                  models.CatalogDriftFieldDrift,
			OpenRailsResourceType: models.CatalogDriftResourceProduct,
			OpenRailsResourceID:   productID.String(),
			StripeResourceID:      "prod_1",
			Field:                 "name",
			DetectedAt:            now,
		}
	}
	if driftDedupeKey(mk()) != driftDedupeKey(mk()) {
		t.Fatal("dedupe key must be stable for the same divergence")
	}
	other := mk()
	other.Field = "description"
	if driftDedupeKey(mk()) == driftDedupeKey(other) {
		t.Fatal("dedupe key must differ across distinct fields")
	}
}
