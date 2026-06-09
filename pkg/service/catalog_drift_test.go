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

// catalogStatusFromActive maps provider active booleans to lifecycle status.
func catalogStatusFromActive(active bool) models.CatalogStatus {
	if active {
		return models.CatalogStatusActive
	}
	return models.CatalogStatusArchived
}

// helper builders
func prod(id uuid.UUID, name, desc string, active bool) *models.Product {
	return &models.Product{ID: id, Slug: "prod-slug", DisplayName: name, Description: desc, Status: catalogStatusFromActive(active)}
}

func price(id, productID uuid.UUID, _ string, amount int64, currency string, active bool, stripePriceID, stripeProductID string) *models.Price {
	p := &models.Price{ID: id, ProductID: productID, Amount: amount, Currency: currency, Status: catalogStatusFromActive(active)}
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

// nmiPrice builds an OpenRails price linked to an NMI plan via the mobius
// processor map.
func nmiPrice(id, productID uuid.UUID, _ string, amount int64, planID string) *models.Price {
	p := &models.Price{ID: id, ProductID: productID, Amount: amount, Currency: "usd", Status: models.CatalogStatusActive}
	if planID != "" {
		p.Processors = map[string]map[string]string{
			string(models.ProcessorMobius): {models.ProcessorKeyPlanID: planID, models.ProcessorKeyProvider: "mobius"},
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

func snapFromRows(products []*models.Product, prices []*models.Price) localCatalogSnapshot {
	return buildSnapshotFromRows(products, prices)
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
	snap := snapFromRows(nil, nil)
	events := computeCatalogDrift(stripeProducts, stripePrices, snap, now)
	if got := countKind(events, models.CatalogDriftOrphanInStripe); got != 2 {
		t.Fatalf("expected 2 orphan events, got %d (events=%+v)", got, events)
	}
	for _, e := range events {
		if e.Provider != models.CatalogDriftProviderStripe {
			t.Fatalf("expected provider=stripe, got %q", e.Provider)
		}
	}
}

func TestComputeCatalogDriftMissingInStripe(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	// OpenRails believes it owns stripe price/product ids that are NOT in the pull.
	products := []*models.Product{prod(productID, "P", "", true)}
	prices := []*models.Price{price(priceID, productID, "Monthly", 1000, "usd", true, "price_gone", "prod_gone")}
	snap := snapFromRows(products, prices)
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
	products := []*models.Product{prod(productID, "Premium", "old desc", true)}
	prices := []*models.Price{price(priceID, productID, "Monthly", 1000, "usd", true, "price_1", "prod_1")}

	stripeProducts := []catalog.StripeProduct{
		{ID: "prod_1", Name: "Premium Plus", Description: "new desc", Active: false, Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsProductKey: "prod-slug",
		}},
	}
	stripePrices := []catalog.StripePrice{
		{ID: "price_1", UnitAmount: 2000, Currency: "eur", Active: false, Nickname: "Yearly", Metadata: map[string]string{
			// Content key reverse-matches the LOCAL row's financial key
			// (prod-slug, usd, 1000, one-time); the Stripe object's own
			// amount/currency/active diverge -> field drift.
			catalog.StripeMetadataOpenRailsPriceKey: "prod-slug.usd.1000.onetime",
		}},
	}
	snap := snapFromRows(products, prices)
	events := computeCatalogDrift(stripeProducts, stripePrices, snap, now)
	driftCount := countKind(events, models.CatalogDriftFieldDrift)
	// product: name, description, active = 3. price: unit_amount, currency, active = 3.
	if driftCount != 6 {
		t.Fatalf("expected 6 field_drift events, got %d (events=%+v)", driftCount, events)
	}
	if countKind(events, models.CatalogDriftMissingInStripe) != 0 {
		t.Fatalf("expected no missing events when stripe ids are present")
	}
}

func TestComputeCatalogDriftNoDriftWhenInSync(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	products := []*models.Product{prod(productID, "Premium", "desc", true)}
	prices := []*models.Price{price(priceID, productID, "Monthly", 1000, "usd", true, "price_1", "prod_1")}

	stripeProducts := []catalog.StripeProduct{
		{ID: "prod_1", Name: "Premium", Description: "desc", Active: true, Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsProductKey: "prod-slug",
		}},
	}
	stripePrices := []catalog.StripePrice{
		{ID: "price_1", UnitAmount: 1000, Currency: "usd", Active: true, Nickname: "Monthly", Metadata: map[string]string{
			catalog.StripeMetadataOpenRailsPriceKey: "prod-slug.usd.1000.onetime",
		}},
	}
	snap := snapFromRows(products, prices)
	events := computeCatalogDrift(stripeProducts, stripePrices, snap, now)
	if len(events) != 0 {
		t.Fatalf("expected zero drift events when fully in sync, got %d: %+v", len(events), events)
	}
}

// TestDriftDedupeKeyStable asserts the dedupe key is identical across reruns for
// the same logical divergence, which is what makes persistence idempotent. The
// provider must be part of the key so the shared field_drift kind never
// collides across Stripe and NMI.
func TestDriftDedupeKeyStable(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	mk := func() models.CatalogDriftEvent {
		return models.CatalogDriftEvent{
			Provider:              models.CatalogDriftProviderStripe,
			Kind:                  models.CatalogDriftFieldDrift,
			OpenRailsResourceType: models.CatalogDriftResourceProduct,
			OpenRailsResourceID:   productID.String(),
			ExternalResourceID:    "prod_1",
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
	// Same divergence, different provider -> distinct key.
	nmiVariant := mk()
	nmiVariant.Provider = models.CatalogDriftProviderNMI
	if driftDedupeKey(mk()) == driftDedupeKey(nmiVariant) {
		t.Fatal("dedupe key must differ across providers for the same field_drift")
	}
}

// ---------------------------------------------------------------------------
// NMI pass tests
// ---------------------------------------------------------------------------

const nmiPlansXMLFixture = `<?xml version="1.0" encoding="UTF-8"?>
<nm_response>
  <plan>
    <plan_id>premium-usd-999-30</plan_id>
    <plan_name>Premium Monthly</plan_name>
    <plan_amount>9.99</plan_amount>
  </plan>
  <plan>
    <plan_id>operator-handmade-plan</plan_id>
    <plan_name>Legacy Plan</plan_name>
    <plan_amount>4.99</plan_amount>
  </plan>
</nm_response>`

func TestParseNMIPlans(t *testing.T) {
	plans, err := parseNMIPlans(nmiPlansXMLFixture)
	if err != nil {
		t.Fatalf("parseNMIPlans: %v", err)
	}
	if len(plans) != 2 {
		t.Fatalf("expected 2 plans, got %d", len(plans))
	}
	if plans[0].AmountCents != 999 {
		t.Fatalf("expected 999 cents for 9.99, got %d", plans[0].AmountCents)
	}
	if plans[1].AmountCents != 499 {
		t.Fatalf("expected 499 cents for 4.99, got %d", plans[1].AmountCents)
	}
}

func TestComputeNMIDriftOrphanContentAddressed(t *testing.T) {
	now := time.Now().UTC()
	plans := []nmiPlan{
		// Content-addressed generated id (no openrails-/UUID prefix) ...
		{PlanID: "premium-usd-999-30", PlanName: "Premium", AmountCents: 999},
		// ... and an operator-hand-made id.
		{PlanID: "operator-handmade", PlanName: "Legacy", AmountCents: 499},
	}
	// No OpenRails prices reference any of these plans -> both orphan_in_nmi.
	snap := snapFromRows(nil, nil)
	events := computeNMICatalogDrift(plans, snap, now)
	if got := countKind(events, models.CatalogDriftOrphanInNMI); got != 2 {
		t.Fatalf("expected 2 orphan_in_nmi, got %d (%+v)", got, events)
	}
	// Content-addressed plan_ids no longer encode a price UUID, so an orphan
	// carries only the external plan_id and never a recovered OpenRailsResourceID.
	for _, e := range events {
		if e.OpenRailsResourceID != "" {
			t.Fatalf("orphan %q must not carry a recovered openrails id, got %q", e.ExternalResourceID, e.OpenRailsResourceID)
		}
	}
}

func TestComputeNMIDriftMissingInNMI(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	planID := "premium-usd-999-30"
	prices := []*models.Price{nmiPrice(priceID, productID, "Premium", 999, planID)}
	snap := snapFromRows(nil, prices)
	// NMI returns no plans -> the price's plan is missing_in_nmi.
	events := computeNMICatalogDrift(nil, snap, now)
	if got := countKind(events, models.CatalogDriftMissingInNMI); got != 1 {
		t.Fatalf("expected 1 missing_in_nmi, got %d (%+v)", got, events)
	}
}

func TestComputeNMIDriftFieldDrift(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	planID := "premium-usd-999-30"
	prices := []*models.Price{nmiPrice(priceID, productID, "Premium", 999, planID)}
	snap := snapFromRows(nil, prices)
	// NMI plan disagrees on amount (plan_name is no longer tracked for prices).
	plans := []nmiPlan{{PlanID: planID, PlanName: "Premium Plus", AmountCents: 1999}}
	events := computeNMICatalogDrift(plans, snap, now)
	if got := countKind(events, models.CatalogDriftFieldDrift); got != 1 {
		t.Fatalf("expected 1 field_drift (plan_amount), got %d (%+v)", got, events)
	}
	for _, e := range events {
		if e.Provider != models.CatalogDriftProviderNMI {
			t.Fatalf("expected provider=nmi, got %q", e.Provider)
		}
	}
}

func TestComputeNMIDriftNoDriftWhenInSync(t *testing.T) {
	now := time.Now().UTC()
	productID := uuid.New()
	priceID := uuid.New()
	planID := "premium-usd-999-30"
	prices := []*models.Price{nmiPrice(priceID, productID, "Premium", 999, planID)}
	snap := snapFromRows(nil, prices)
	plans := []nmiPlan{{PlanID: planID, PlanName: "Premium", AmountCents: 999}}
	events := computeNMICatalogDrift(plans, snap, now)
	if len(events) != 0 {
		t.Fatalf("expected zero NMI drift when in sync, got %d: %+v", len(events), events)
	}
}
