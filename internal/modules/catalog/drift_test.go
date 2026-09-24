package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

type pagedStripeLister struct {
	products, prices [][]string
	productCalls     int
	priceCalls       int
}

func nextPage(pages [][]string, call int) ([]string, string) {
	if call >= len(pages) {
		return nil, ""
	}
	if call+1 < len(pages) {
		return pages[call], pages[call][len(pages[call])-1]
	}
	return pages[call], ""
}

func (f *pagedStripeLister) ListProducts(_ context.Context, after string) ([]StripeProduct, string, error) {
	ids, next := nextPage(f.products, f.productCalls)
	f.productCalls++
	out := make([]StripeProduct, len(ids))
	for i, id := range ids {
		out[i] = StripeProduct{ID: id}
	}
	return out, next, nil
}

func (f *pagedStripeLister) ListPrices(_ context.Context, after string) ([]StripePrice, string, error) {
	ids, next := nextPage(f.prices, f.priceCalls)
	f.priceCalls++
	out := make([]StripePrice, len(ids))
	for i, id := range ids {
		out[i] = StripePrice{ID: id}
	}
	return out, next, nil
}

func TestFetchStripeCatalogReadsEveryPage(t *testing.T) {
	lister := &pagedStripeLister{products: [][]string{{"prod_1", "prod_2"}, {"prod_3"}}, prices: [][]string{{"price_1"}, {"price_2", "price_3"}}}
	products, prices, err := FetchStripeCatalog(t.Context(), lister)
	require.NoError(t, err)
	require.Len(t, products, 3)
	require.Len(t, prices, 3)
	require.Equal(t, []int{2, 2}, []int{lister.productCalls, lister.priceCalls})
}

func stripePrice(productID, psp uuid.UUID, amount int64, currency, priceID, prodID string) *models.Price {
	return &models.Price{ID: uuid.New(), ProductID: productID, Amount: amount, Currency: currency, PSPLinks: map[string]map[string]string{
		"stripe": {models.RailKeyRail: "stripe", models.RailKeyPSPID: psp.String(), models.RailKeyStripePriceID: priceID, models.RailKeyStripeProductID: prodID},
	}}
}

func nmiPrice(psp uuid.UUID, key string, amount int64, planID string) *models.Price {
	return &models.Price{ID: uuid.New(), ProductID: uuid.New(), Amount: amount, Currency: "USD", PSPLinks: map[string]map[string]string{
		key: {models.RailKeyRail: string(models.RailNMI), models.RailKeyPSPID: psp.String(), models.RailKeyPlanID: planID},
	}}
}

func countKinds(events []models.CatalogDriftEvent) map[models.CatalogDriftKind]int {
	out := map[models.CatalogDriftKind]int{}
	for _, e := range events {
		out[e.Kind]++
	}
	return out
}

func TestComputeStripeDrift(t *testing.T) {
	now := time.Now().UTC()
	psp, productID := uuid.New(), uuid.New()
	meta := func(k, v string) map[string]string { return map[string]string{k: v} }
	product := &models.Product{ID: productID, Key: "prod-key", DisplayName: "Premium", Description: "old"}
	price := stripePrice(productID, psp, 10_000_000, "usd", "price_1", "prod_1")
	snap := BuildDriftSnapshot([]*models.Product{product}, []*models.Price{price}, psp)
	syncedProduct := StripeProduct{ID: "prod_1", Name: "Premium", Description: "old", Active: true, Metadata: meta(StripeMetadataOpenRailsProductKey, "prod-key")}
	syncedPrice := StripePrice{ID: "price_1", UnitAmount: 1000, Currency: "USD", Active: true, Metadata: meta(StripeMetadataOpenRailsPriceKey, "prod-key.usd.10000000.onetime")}

	// Stripe cents compare against local micros.
	require.Empty(t, ComputeStripeDrift([]StripeProduct{syncedProduct}, []StripePrice{syncedPrice}, snap, now))

	// The lookup_key marker is an equivalent ownership marker.
	byLookup := syncedPrice
	byLookup.Metadata, byLookup.LookupKey = nil, "openrails.prod-key.usd.10000000.onetime"
	require.Empty(t, ComputeStripeDrift([]StripeProduct{syncedProduct}, []StripePrice{byLookup}, snap, now))

	require.Equal(t, map[models.CatalogDriftKind]int{models.CatalogDriftMissingInStripe: 2}, countKinds(ComputeStripeDrift(nil, nil, snap, now)))

	unowned := ComputeStripeDrift([]StripeProduct{{ID: "prod_native", Active: true}},
		[]StripePrice{{ID: "price_ghost", Active: true, Metadata: meta(StripeMetadataOpenRailsPriceKey, "missing.usd.1.onetime")}},
		BuildDriftSnapshot(nil, nil, psp), now)
	require.Equal(t, map[models.CatalogDriftKind]int{models.CatalogDriftOrphanInStripe: 2}, countKinds(unowned))

	drifted := ComputeStripeDrift(
		[]StripeProduct{{ID: "prod_1", Name: "Premium Plus", Description: "new", Metadata: syncedProduct.Metadata}},
		[]StripePrice{{ID: "price_1", UnitAmount: 2000, Currency: "EUR", Metadata: syncedPrice.Metadata}}, snap, now)
	fields := map[string]int{}
	for _, e := range drifted {
		require.Equal(t, models.CatalogDriftFieldDrift, e.Kind)
		fields[string(e.OpenRailsResourceType)+"."+e.Field]++
	}
	require.Equal(t, map[string]int{
		string(models.CatalogDriftResourceProduct) + ".name": 1, string(models.CatalogDriftResourceProduct) + ".description": 1,
		string(models.CatalogDriftResourceProduct) + ".active": 1, string(models.CatalogDriftResourcePrice) + ".unit_amount": 1,
		string(models.CatalogDriftResourcePrice) + ".currency": 1, string(models.CatalogDriftResourcePrice) + ".active": 1,
	}, fields)

	// Zero-decimal currencies widen with their own native scale.
	jpy := stripePrice(productID, psp, 1_230_000, "JPY", "price_jpy", "prod_1")
	jpySnap := BuildDriftSnapshot([]*models.Product{product}, []*models.Price{jpy}, psp)
	require.Empty(t, ComputeStripeDrift([]StripeProduct{syncedProduct},
		[]StripePrice{{ID: "price_jpy", UnitAmount: 123, Currency: "JPY", Active: true, Metadata: meta(StripeMetadataOpenRailsPriceKey, "prod-key.jpy.1230000.onetime")}}, jpySnap, now))
}

func TestComputeNMIDrift(t *testing.T) {
	now := time.Now().UTC()
	psp := uuid.New()
	// FAB-6: an unparseable amount is skipped, never read as a zero price.
	plans := MapNMIPlans([]nmi.V5Plan{{ID: "premium-usd-999-30", PlanAmount: "9.99"}, {ID: "bad", PlanAmount: "nope"}, {ID: "handmade", PlanAmount: "4.99"}})
	require.Equal(t, []NMIPlan{{PlanID: "premium-usd-999-30", AmountCents: 999}, {PlanID: "handmade", AmountCents: 499}}, plans)

	orphans := ComputeNMIDrift(plans, BuildDriftSnapshot(nil, nil, psp), now)
	require.Equal(t, map[models.CatalogDriftKind]int{models.CatalogDriftOrphanInNMI: 2}, countKinds(orphans))

	price := nmiPrice(psp, "mobius", 9_990_000, "premium-usd-999-30")
	snap := BuildDriftSnapshot(nil, []*models.Price{price}, psp)
	require.Empty(t, ComputeNMIDrift([]NMIPlan{{PlanID: "premium-usd-999-30", AmountCents: 999}}, snap, now))
	missing := ComputeNMIDrift(nil, snap, now)
	require.Len(t, missing, 1)
	require.Equal(t, models.CatalogDriftMissingInNMI, missing[0].Kind)
	require.Equal(t, price.ID.String(), missing[0].OpenRailsResourceID)
	amount := ComputeNMIDrift([]NMIPlan{{PlanID: "premium-usd-999-30", AmountCents: 1999}}, snap, now)
	require.Len(t, amount, 1)
	require.Equal(t, []string{"plan_amount", "9990000", "19990000"}, []string{amount[0].Field, amount[0].OpenRailsValue, amount[0].ExternalValue})
}

// #993: links bound to another account are neither evidence nor expectations
// for the account being read; the zero account is the all-accounts view.
func TestDriftSnapshotIsScopedToTheReadAccount(t *testing.T) {
	now := time.Now().UTC()
	a, b := uuid.New(), uuid.New()
	prices := []*models.Price{nmiPrice(a, "primary", 9_990_000, "plan-a"), nmiPrice(b, "secondary", 4_990_000, "plan-b")}
	require.Empty(t, ComputeNMIDrift([]NMIPlan{{PlanID: "plan-a", AmountCents: 999}}, BuildDriftSnapshot(nil, prices, a), now))
	events := ComputeNMIDrift(nil, BuildDriftSnapshot(nil, prices, b), now)
	require.Len(t, events, 1)
	require.Equal(t, prices[1].ID.String(), events[0].OpenRailsResourceID)
	require.Len(t, BuildDriftSnapshot(nil, prices, uuid.Nil).NMIPlanByPriceID, 2)
}

func TestExtrasIndexAndContentKeys(t *testing.T) {
	days := func(d int) *int { return &d }
	require.Equal(t, "prod.usd.9990000.onetime", OpenRailsPriceContentKey(" prod ", " USD ", 9_990_000, nil))
	require.Equal(t, "prod.usd.9990000.onetime", OpenRailsPriceContentKey("prod", "usd", 9_990_000, days(0)))
	require.Equal(t, "prod.jpy.1230000.30", OpenRailsPriceContentKey("prod", "JPY", 1_230_000, days(30)))
	require.Equal(t, "k", RemoteStripePriceContentKey(StripePrice{Metadata: map[string]string{StripeMetadataOpenRailsPriceKey: " k "}, LookupKey: "openrails.other"}), "metadata wins")
	require.Equal(t, "other", RemoteStripePriceContentKey(StripePrice{LookupKey: "openrails.other"}))
	require.Empty(t, RemoteStripePriceContentKey(StripePrice{LookupKey: "native"}))

	productID := uuid.New()
	hours := 30 * 24
	recurring := stripePrice(productID, uuid.New(), 9_990_000, "USD", "price_linked", "prod_linked")
	recurring.AutoRenew, recurring.AccessDurationHours = true, &hours
	ix := BuildExtrasIndex([]*models.Product{{ID: productID, Key: "prod"}}, []*models.Price{recurring})
	for _, tc := range []struct {
		price StripePrice
		extra bool
	}{
		{StripePrice{ID: "price_linked"}, false},
		{StripePrice{ID: "price_x", LookupKey: "openrails.prod.usd.9990000.30"}, false},
		{StripePrice{ID: "price_x", LookupKey: "openrails.prod.usd.9990000.onetime"}, true},
		{StripePrice{ID: "price_native"}, true},
	} {
		extra, _ := ix.StripePriceExtra(tc.price)
		require.Equal(t, tc.extra, extra, "%+v", tc.price)
	}
	for _, tc := range []struct {
		product StripeProduct
		extra   bool
	}{
		{StripeProduct{ID: "prod_linked"}, false},
		{StripeProduct{ID: "prod_x", Metadata: map[string]string{StripeMetadataOpenRailsProductKey: "prod"}}, false},
		{StripeProduct{ID: "prod_x", Metadata: map[string]string{StripeMetadataOpenRailsProductKey: "gone"}}, true},
	} {
		extra, _ := ix.StripeProductExtra(tc.product)
		require.Equal(t, tc.extra, extra, "%+v", tc.product)
	}
	require.Equal(t, [][2]any{{"week", 1}, {"month", 1}, {"year", 1}, {"day", 90}, {"month", 1}},
		[][2]any{interval(7), interval(30), interval(365), interval(90), interval(0)})
}

func interval(days int) [2]any {
	unit, count := StripeIntervalForDays(days)
	return [2]any{unit, count}
}
