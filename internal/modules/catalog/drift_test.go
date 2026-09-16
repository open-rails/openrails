package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

type pagedStripeLister struct {
	productPages [][]StripeProduct
	pricePages   [][]StripePrice
	productCalls int
	priceCalls   int
}

func page[T any](pages [][]T, call int, id func(T) string) ([]T, string) {
	if call >= len(pages) {
		return nil, ""
	}
	next := ""
	if call+1 < len(pages) && len(pages[call]) > 0 {
		next = id(pages[call][len(pages[call])-1])
	}
	return pages[call], next
}

func (f *pagedStripeLister) ListProducts(context.Context, string) ([]StripeProduct, string, error) {
	items, next := page(f.productPages, f.productCalls, func(p StripeProduct) string { return p.ID })
	f.productCalls++
	return items, next, nil
}

func (f *pagedStripeLister) ListPrices(context.Context, string) ([]StripePrice, string, error) {
	items, next := page(f.pricePages, f.priceCalls, func(p StripePrice) string { return p.ID })
	f.priceCalls++
	return items, next, nil
}

func TestFetchStripeCatalogPaginates(t *testing.T) {
	lister := &pagedStripeLister{
		productPages: [][]StripeProduct{{{ID: "prod_1"}, {ID: "prod_2"}}, {{ID: "prod_3"}}},
		pricePages:   [][]StripePrice{{{ID: "price_1"}}, {{ID: "price_2"}, {ID: "price_3"}}},
	}
	products, prices, err := FetchStripeCatalog(context.Background(), lister)
	require.NoError(t, err)
	require.Len(t, products, 3)
	require.Len(t, prices, 3)
	require.Equal(t, 2, lister.productCalls)
	require.Equal(t, 2, lister.priceCalls)
}

func driftProduct(id uuid.UUID, name, desc string, active bool) *models.Product {
	return &models.Product{ID: id, Key: "prod-key", DisplayName: name, Description: desc, Archived: !active}
}

func stripeLinkedPrice(id, productID, pspID uuid.UUID, amount int64, stripePriceID, stripeProductID string) *models.Price {
	return &models.Price{ID: id, ProductID: productID, Amount: amount, Currency: "usd", PSPLinks: map[string]map[string]string{
		"stripe": {models.RailKeyRail: "stripe", models.RailKeyPSPID: pspID.String(),
			models.RailKeyStripePriceID: stripePriceID, models.RailKeyStripeProductID: stripeProductID},
	}}
}

func nmiLinkedPrice(id, pspID uuid.UUID, key string, amount int64, planID string) *models.Price {
	return &models.Price{ID: id, ProductID: uuid.New(), Amount: amount, Currency: "USD", PSPLinks: map[string]map[string]string{
		key: {models.RailKeyRail: string(models.RailNMI), models.RailKeyPSPID: pspID.String(), models.RailKeyPlanID: planID},
	}}
}

func kinds(events []models.CatalogDriftEvent, kind models.CatalogDriftKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func TestComputeStripeDriftOrphanMissingAndFields(t *testing.T) {
	now := time.Now().UTC()
	psp := uuid.New()
	events := ComputeStripeDrift([]StripeProduct{{ID: "prod_native", Active: true}},
		[]StripePrice{{ID: "price_ghost", Active: true, Metadata: map[string]string{StripeMetadataOpenRailsPriceKey: "missing.usd.1.onetime"}}},
		BuildDriftSnapshot(nil, nil, psp), now)
	require.Equal(t, 2, kinds(events, models.CatalogDriftOrphanInStripe))

	productID, priceID := uuid.New(), uuid.New()
	products := []*models.Product{driftProduct(productID, "Premium", "old", true)}
	prices := []*models.Price{stripeLinkedPrice(priceID, productID, psp, 10_000_000, "price_1", "prod_1")}
	snap := BuildDriftSnapshot(products, prices, psp)
	require.Equal(t, 2, kinds(ComputeStripeDrift(nil, nil, snap, now), models.CatalogDriftMissingInStripe))

	events = ComputeStripeDrift(
		[]StripeProduct{{ID: "prod_1", Name: "Premium Plus", Description: "new", Metadata: map[string]string{StripeMetadataOpenRailsProductKey: "prod-key"}}},
		[]StripePrice{{ID: "price_1", UnitAmount: 2000, Currency: "EUR", Metadata: map[string]string{StripeMetadataOpenRailsPriceKey: "prod-key.usd.10000000.onetime"}}},
		snap, now)
	require.Equal(t, 6, kinds(events, models.CatalogDriftFieldDrift), "%+v", events)
	require.Zero(t, kinds(events, models.CatalogDriftMissingInStripe))

	inSync := ComputeStripeDrift(
		[]StripeProduct{{ID: "prod_1", Name: "Premium", Description: "old", Active: true, Metadata: map[string]string{StripeMetadataOpenRailsProductKey: "prod-key"}}},
		[]StripePrice{{ID: "price_1", UnitAmount: 1000, Currency: "USD", Active: true, Metadata: map[string]string{StripeMetadataOpenRailsPriceKey: "prod-key.usd.10000000.onetime"}}},
		snap, now)
	require.Empty(t, inSync, "Stripe cents must compare against local micros")
}

// Links bound to another account are neither evidence nor expected objects for
// the account being read (#993 immutable PSP identity).
func TestDriftSnapshotUsesOnlyTheReadAccountsLinks(t *testing.T) {
	now := time.Now().UTC()
	accountA, accountB := uuid.New(), uuid.New()
	priceA, priceB := uuid.New(), uuid.New()
	prices := []*models.Price{
		nmiLinkedPrice(priceA, accountA, "primary", 9_990_000, "plan-a"),
		nmiLinkedPrice(priceB, accountB, "secondary", 4_990_000, "plan-b"),
	}
	events := ComputeNMIDrift([]NMIPlan{{PlanID: "plan-a", AmountCents: 999}}, BuildDriftSnapshot(nil, prices, accountA), now)
	require.Empty(t, events, "account B's plan must not be reported missing from account A")

	events = ComputeNMIDrift(nil, BuildDriftSnapshot(nil, prices, accountB), now)
	require.Len(t, events, 1)
	require.Equal(t, models.CatalogDriftMissingInNMI, events[0].Kind)
	require.Equal(t, priceB.String(), events[0].OpenRailsResourceID)

	union := BuildDriftSnapshot(nil, prices, uuid.Nil)
	require.Len(t, union.NMIPlanByPriceID, 2, "the zero account is the all-accounts extras view")
}

func TestComputeNMIDrift(t *testing.T) {
	now := time.Now().UTC()
	psp, priceID := uuid.New(), uuid.New()
	plans := MapNMIPlans([]nmi.V5Plan{{ID: "premium-usd-999-30", PlanAmount: "9.99"}, {ID: "bad", PlanAmount: "nope"}, {ID: "handmade", PlanAmount: "4.99"}})
	require.Equal(t, []NMIPlan{{PlanID: "premium-usd-999-30", AmountCents: 999}, {PlanID: "handmade", AmountCents: 499}}, plans)

	orphans := ComputeNMIDrift(plans, BuildDriftSnapshot(nil, nil, psp), now)
	require.Equal(t, 2, kinds(orphans, models.CatalogDriftOrphanInNMI))
	for _, e := range orphans {
		require.Empty(t, e.OpenRailsResourceID)
	}
	snap := BuildDriftSnapshot(nil, []*models.Price{nmiLinkedPrice(priceID, psp, "mobius", 9_990_000, "premium-usd-999-30")}, psp)
	require.Equal(t, 1, kinds(ComputeNMIDrift(nil, snap, now), models.CatalogDriftMissingInNMI))
	require.Equal(t, 1, kinds(ComputeNMIDrift([]NMIPlan{{PlanID: "premium-usd-999-30", AmountCents: 1999}}, snap, now), models.CatalogDriftFieldDrift))
	require.Empty(t, ComputeNMIDrift([]NMIPlan{{PlanID: "premium-usd-999-30", AmountCents: 999}}, snap, now))
}
