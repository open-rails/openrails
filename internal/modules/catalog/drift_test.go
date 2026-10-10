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

	unknownCurrency := syncedPrice
	unknownCurrency.Currency = "XXQ"
	events := ComputeStripeDrift([]StripeProduct{syncedProduct}, []StripePrice{unknownCurrency}, snap, now)
	require.Len(t, events, 1)
	require.Equal(t, models.CatalogDriftFieldDrift, events[0].Kind)
	require.Equal(t, []string{"currency", "usd", "XXQ"}, []string{events[0].Field, events[0].OpenRailsValue, events[0].ExternalValue})
}

func TestComputeNMIDrift(t *testing.T) {
	now := time.Now().UTC()
	psp := uuid.New()
	plans := MapNMIPlans([]nmi.V5Plan{{ID: "premium-usd-999-30", PlanAmount: "9.99"}, {ID: "bad", PlanAmount: "nope"}, {ID: "handmade", PlanAmount: " 4.99 "}})
	require.Equal(t, []NMIPlan{{PlanID: "premium-usd-999-30", Amount: "9.99"}, {PlanID: "bad", Amount: "nope"}, {PlanID: "handmade", Amount: "4.99"}}, plans)

	orphans := ComputeNMIDrift(plans, BuildDriftSnapshot(nil, nil, psp), now)
	require.Equal(t, map[models.CatalogDriftKind]int{models.CatalogDriftOrphanInNMI: 3}, countKinds(orphans))

	price := nmiPrice(psp, "mobius", 9_990_000, "premium-usd-999-30")
	snap := BuildDriftSnapshot(nil, []*models.Price{price}, psp)
	require.Empty(t, ComputeNMIDrift([]NMIPlan{{PlanID: "premium-usd-999-30", Amount: "9.99"}}, snap, now))
	missing := ComputeNMIDrift(nil, snap, now)
	require.Len(t, missing, 1)
	require.Equal(t, models.CatalogDriftMissingInNMI, missing[0].Kind)
	require.Equal(t, price.ID.String(), missing[0].OpenRailsResourceID)
	amount := ComputeNMIDrift([]NMIPlan{{PlanID: "premium-usd-999-30", Amount: "19.99"}}, snap, now)
	require.Len(t, amount, 1)
	require.Equal(t, []string{"plan_amount", "9990000", "19990000"}, []string{amount[0].Field, amount[0].OpenRailsValue, amount[0].ExternalValue})
	// An unparseable linked amount is drift reported verbatim, never a zero price.
	bad := ComputeNMIDrift([]NMIPlan{{PlanID: "premium-usd-999-30", Amount: "nope"}}, snap, now)
	require.Len(t, bad, 1)
	require.Equal(t, []string{"plan_amount", "9990000", "nope"}, []string{bad[0].Field, bad[0].OpenRailsValue, bad[0].ExternalValue})

	// NMI writes 500 yen as "500.00": read in the price's currency it is ¥500.
	yen := nmiPrice(psp, "mobius", 5_000_000, "premium-jpy-500-30")
	yen.Currency = "JPY"
	yenSnap := BuildDriftSnapshot(nil, []*models.Price{yen}, psp)
	require.Empty(t, ComputeNMIDrift([]NMIPlan{{PlanID: "premium-jpy-500-30", Amount: "500.00"}}, yenSnap, now))
	fractional := ComputeNMIDrift([]NMIPlan{{PlanID: "premium-jpy-500-30", Amount: "500.50"}}, yenSnap, now)
	require.Len(t, fractional, 1)
	require.Equal(t, []string{"plan_amount", "5000000", "500.50"}, []string{fractional[0].Field, fractional[0].OpenRailsValue, fractional[0].ExternalValue})
}

// Links bound to another account are neither evidence nor expectations for
// the account being read; the zero account is the all-accounts view.
func TestDriftSnapshotIsScopedToTheReadAccount(t *testing.T) {
	now := time.Now().UTC()
	a, b := uuid.New(), uuid.New()
	prices := []*models.Price{nmiPrice(a, "primary", 9_990_000, "plan-a"), nmiPrice(b, "secondary", 4_990_000, "plan-b")}
	require.Empty(t, ComputeNMIDrift([]NMIPlan{{PlanID: "plan-a", Amount: "9.99"}}, BuildDriftSnapshot(nil, prices, a), now))
	events := ComputeNMIDrift(nil, BuildDriftSnapshot(nil, prices, b), now)
	require.Len(t, events, 1)
	require.Equal(t, prices[1].ID.String(), events[0].OpenRailsResourceID)
	require.Len(t, BuildDriftSnapshot(nil, prices, uuid.Nil).NMIPlanByPriceID, 2)
}

func TestStripeCatalogIndexesRetainEveryAccount(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	product := &models.Product{ID: uuid.New(), Key: "premium"}
	price := stripePrice(product.ID, a, 9_990_000, "USD", "price_a", "prod_a")
	price.PSPLinks["secondary"] = map[string]string{
		models.RailKeyRail: "stripe", models.RailKeyPSPID: b.String(),
		models.RailKeyStripePriceID: "price_b", models.RailKeyStripeProductID: "prod_b",
	}
	for _, tc := range []struct {
		name string
		psp  uuid.UUID
		ids  []string
	}{
		{"all accounts", uuid.Nil, []string{"a", "b"}},
		{"first account", a, []string{"a"}},
		{"second account", b, []string{"b"}},
		{"unlinked account", uuid.New(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snap := BuildDriftSnapshot([]*models.Product{product}, []*models.Price{price}, tc.psp)
			require.Len(t, snap.StripeProductIDs, len(tc.ids))
			require.Len(t, snap.StripePriceIDs, len(tc.ids))
			for _, id := range tc.ids {
				require.Equal(t, product.ID.String(), snap.StripeProductIDs["prod_"+id])
				require.Equal(t, price.ID.String(), snap.StripePriceIDs["price_"+id])
			}
		})
	}
}

func TestRemotePriceKeyAndInterval(t *testing.T) {
	require.Equal(t, "k", RemoteStripePriceKey(StripePrice{Metadata: map[string]string{StripeMetadataOpenRailsPriceKey: " k "}, LookupKey: "openrails.other"}), "metadata wins")
	require.Equal(t, "other", RemoteStripePriceKey(StripePrice{LookupKey: "openrails.other"}))
	require.Empty(t, RemoteStripePriceKey(StripePrice{LookupKey: "native"}))
	require.Equal(t, [][2]any{{"week", 1}, {"month", 1}, {"year", 1}, {"day", 90}, {"month", 1}},
		[][2]any{interval(7), interval(30), interval(365), interval(90), interval(0)})
}

func interval(days int) [2]any {
	unit, count := StripeIntervalForDays(days)
	return [2]any{unit, count}
}

func TestStripeDriftKeepsSameMoneyPriceIdentities(t *testing.T) {
	product := &models.Product{ID: uuid.New(), Key: "premium"}
	first := &models.Price{ID: uuid.New(), ProductID: product.ID, Key: "monthly", Amount: 10_000_000, Currency: "USD"}
	second := &models.Price{ID: uuid.New(), ProductID: product.ID, Key: "special", Amount: 10_000_000, Currency: "USD", Archived: true}
	rows := []*models.Price{first, second}
	snap := BuildDriftSnapshot([]*models.Product{product}, rows, uuid.Nil)
	remote := []StripePrice{
		{ID: "price_first", UnitAmount: 1000, Currency: "usd", Active: true, LookupKey: "openrails." + first.ID.String()},
		{ID: "price_second", UnitAmount: 1000, Currency: "usd", Metadata: map[string]string{StripeMetadataOpenRailsPriceID: second.ID.String(), StripeMetadataOpenRailsPriceKey: "premium.usd.10000000.onetime"}},
	}
	require.Empty(t, ComputeStripeDrift(nil, remote, snap, time.Now()))
	// A financial-terms marker cannot pick one of several same-money siblings.
	ambiguous := StripePrice{ID: "price_unbound", UnitAmount: 1000, Currency: "usd", LookupKey: "openrails.premium.usd.10000000.onetime"}
	events := ComputeStripeDrift(nil, []StripePrice{ambiguous}, snap, time.Now())
	require.Len(t, events, 1)
	require.Equal(t, models.CatalogDriftOrphanInStripe, events[0].Kind)
}
