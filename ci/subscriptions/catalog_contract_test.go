//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// staffCall calls a route with the staff credential and an optional JSON
// body, and decodes the response into out when there is one.
func (w *world) staffCall(method, path string, body, out any) int {
	w.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(w.t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.server.URL+mountPrefix+path, reader)
	require.NoError(w.t, err)
	req.Header.Set("Authorization", "Bearer "+w.auth.token(w.t, "staff"))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(w.t, err)
	if out != nil && len(raw) > 0 {
		require.NoError(w.t, json.Unmarshal(raw, out), "%s %s: %s", method, path, raw)
	}
	return res.StatusCode
}

// onSale is the merchant's products on sale named by keys as a host puts them
// in its pages: read with the Go client, their prices as a buyer sees them.
func (w *world) onSale(keys ...string) []billing.Product {
	w.t.Helper()
	page, err := w.client[remote].ListProducts(w.t.Context(), billing.ProductListParams{Keys: keys, Archived: new(false), ForSale: new(true), PageRequest: billing.PageRequest{Limit: len(keys)}})
	require.NoError(w.t, err)
	for i := range page.Items {
		page.Items[i].Prices = billing.BuyerPrices(page.Items[i].Prices)
	}
	return page.Items
}

// One product and one price shape on every route that returns them: the
// merchant's and the Go client's, and a buyer's view of it. Lists are one cursor
// envelope; a product carries its current prices.
func TestCatalogOneShapePerNoun(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.start()
	c := w.client[remote]
	tier := "plans-" + uuid.NewString()[:6]
	hours := monthHours
	var products []*billing.Product
	for i := range 3 {
		product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "plan-" + uuid.NewString()[:8], DisplayName: "Plan", TierGroup: &tier, TierRank: i + 1,
			Entitlements: []string{"content:plan"}})
		require.NoError(t, err)
		require.Empty(t, product.Prices)
		_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: int64(i+1) * 1_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err)
		products = append(products, product)
	}

	// The Go client and the wire agree; a product embeds its current price.
	got, err := c.GetProduct(t.Context(), products[0].ID)
	require.NoError(t, err)
	require.Len(t, got.Prices, 1)
	price := got.Prices[0]
	require.Equal(t, products[0].Key+"-monthly", price.Key, "the default key names the cadence")
	require.Nil(t, price.Provider, "the price is ours")
	var wire map[string]any
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodGet, "/v1/catalog/prices/"+price.ID.String(), nil, &wire))
	for _, field := range []string{"id", "key", "product_id", "archived", "unit_amount", "currency", "access_duration_hours", "billing_interval_hours", "trial_unit_amount", "trial_duration_hours", "sell_via", "provider", "solana_plan", "created_at", "updated_at"} {
		require.Contains(t, wire, field, "nulls are present, never omitted")
	}
	require.NotContains(t, wire, "psps", "a price names no PSPs")
	require.Equal(t, "1000000", wire["unit_amount"])

	// Cursor paging walks every product exactly once.
	seen := map[billing.ProductID]bool{}
	for cursor := ""; ; {
		page, err := c.ListProducts(t.Context(), billing.ProductListParams{TierGroup: tier, PageRequest: billing.PageRequest{Limit: 2, Cursor: cursor}})
		require.NoError(t, err)
		for _, p := range page.Items {
			require.False(t, seen[p.ID])
			seen[p.ID] = true
			require.Len(t, p.Prices, 1)
		}
		if page.Next == "" {
			break
		}
		cursor = page.Next
	}
	require.Len(t, seen, 3)
	var envelope map[string]any
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodGet, "/v1/catalog/prices?limit=1&recurring=true", nil, &envelope))
	require.Len(t, envelope["data"], 1)
	require.NotNil(t, envelope["next_cursor"])
	require.Equal(t, http.StatusBadRequest, w.staffCall(http.MethodGet, "/v1/catalog/prices?cursor=nope", nil, nil))

	// A buyer sees the same objects, on sale only, without PSP identifiers.
	archived := true
	_, err = c.UpdateProduct(t.Context(), products[2].ID, billing.UpdateProductParams{Archived: catalog.Value(archived)})
	require.NoError(t, err)
	keys := map[string]bool{}
	for _, product := range w.onSale(products[0].Key, products[2].Key) {
		keys[product.Key] = true
		if product.Key == products[0].Key {
			require.Len(t, product.Prices, 1, "a product embeds its prices on sale")
		}
		for _, p := range product.Prices {
			require.Nil(t, p.Provider)
			require.Nil(t, p.SolanaPlan, "provider identifiers stay with the merchant")
		}
	}
	require.True(t, keys[products[0].Key])
	require.False(t, keys[products[2].Key], "an archived product is not on sale")

	// Retired routes and fields are gone.
	require.Equal(t, http.StatusNotFound, w.staffCall(http.MethodGet, "/v1/prices", nil, nil), "a product embeds its prices")
	require.Equal(t, http.StatusNotFound, w.staffCall(http.MethodGet, "/v1/products?keys="+products[0].Key, nil, nil), "no public catalog")
	require.Equal(t, http.StatusNotFound, w.staffCall(http.MethodPost, "/v1/catalog/products/"+products[0].ID.String()+"/activate", nil, nil))
	require.Equal(t, http.StatusNotFound, w.staffCall(http.MethodPost, "/v1/catalog/prices/"+price.ID.String()+"/key", map[string]any{"key": "x"}, nil))
	require.Equal(t, http.StatusBadRequest, w.staffCall(http.MethodPatch, "/v1/catalog/products/"+products[0].ID.String(), map[string]any{"set_tier_group": true}, nil))
}

// A product patch is a merge: omitted fields stay, null clears. New financial
// terms create a price version under the same immutable key; patch only changes
// its lifecycle metadata.
func TestCatalogPatchSemantics(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		c := w.client[tp]
		tier := "tier-" + uuid.NewString()[:6]
		product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "p-" + uuid.NewString()[:8], DisplayName: "Before", Description: "kept", TierGroup: &tier, TierRank: 2})
		require.NoError(t, err)
		updated, err := c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("After"), TierGroup: catalog.Null[string]()})
		require.NoError(t, err)
		require.Equal(t, "After", updated.DisplayName)
		require.Equal(t, "kept", updated.Description, "an omitted field keeps its value")
		require.Nil(t, updated.TierGroup, "null clears")
		require.Equal(t, 2, updated.TierRank)
		_, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Null[string]()})
		require.ErrorIs(t, err, billing.ErrInvalid, "a required field cannot be cleared")

		first, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-a", UnitAmount: 1_000_000, Currency: "USD"})
		require.NoError(t, err)
		second, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: first.Key, UnitAmount: 2_000_000, Currency: "USD"})
		require.NoError(t, err)
		require.Equal(t, first.Key, second.Key)
		require.EqualValues(t, 1, first.Version)
		require.EqualValues(t, 2, second.Version)
		require.Equal(t, http.StatusBadRequest, w.staffCall(http.MethodPatch, "/v1/catalog/prices/"+second.ID.String(), map[string]any{"key": "renamed"}, nil), "a key rename cannot rewrite price identity")
		current, err := priceByKey(t.Context(), c, product.Key, first.Key)
		require.NoError(t, err)
		require.Equal(t, second.ID, current.ID)
		displaced, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.True(t, displaced.Archived, "the key's previous holder is archived")

		history, err := priceKeyHistory(t.Context(), c, product.Key, first.Key, billing.PageRequest{Limit: 1})
		require.NoError(t, err)
		require.Len(t, history.Items, 1)
		require.Equal(t, second.ID, history.Items[0].Price.ID, "most recent first")
		require.NotEmpty(t, history.Next)
		rest, err := priceKeyHistory(t.Context(), c, product.Key, first.Key, billing.PageRequest{Cursor: history.Next})
		require.NoError(t, err)
		require.NotEmpty(t, rest.Items)

		restored, err := c.UpdatePrice(t.Context(), first.ID, billing.UpdatePriceParams{Archived: catalog.Value(false)})
		require.NoError(t, err)
		require.False(t, restored.Archived)
		require.Equal(t, first.Version, restored.Version)
	}
}

// Meters and their rate card: a restated meter keeps its card, null removes
// it, and removing an absent card is no change.
func TestMetersAndTheirRateCards(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		c := w.client[tp]
		key := "requests-" + uuid.NewString()[:6]
		meter, err := c.SetMeter(t.Context(), key, billing.SetMeterParams{Aggregation: catalog.AggregationCount})
		require.NoError(t, err)
		require.Equal(t, key, meter.EventType, "the event type defaults to the key")
		require.True(t, meter.BillingSupported)
		require.Nil(t, meter.RateCard)
		_, err = c.SetMeter(t.Context(), key+"-max", billing.SetMeterParams{Aggregation: catalog.AggregationMax, ValueProperty: "$.v"})
		requireCode(t, err, http.StatusBadRequest, "usage_meter_invalid")

		product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "api-" + uuid.NewString()[:8], DisplayName: "API"})
		require.NoError(t, err)
		perUnit := func(amount int64) catalog.RatePrice {
			return catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD", PerUnit: &catalog.PerUnitPrice{UnitAmount: amount}}
		}
		card := billing.MeterRateCardParams{ProductID: product.ID, Price: perUnit(10_000)}
		meter, err = c.SetMeter(t.Context(), key, billing.SetMeterParams{Aggregation: catalog.AggregationCount, RateCard: catalog.Value(card)})
		require.NoError(t, err)
		require.NotNil(t, meter.RateCard)
		require.Equal(t, product.ID, meter.RateCard.ProductID)
		meter, err = c.SetMeter(t.Context(), key, billing.SetMeterParams{Aggregation: catalog.AggregationCount})
		require.NoError(t, err)
		require.NotNil(t, meter.RateCard, "a meter restated without rate_card keeps its rate card")

		removeCard := billing.SetMeterParams{Aggregation: catalog.AggregationCount, RateCard: catalog.Null[billing.MeterRateCardParams]()}
		meter, err = c.SetMeter(t.Context(), key, removeCard)
		require.NoError(t, err)
		require.Nil(t, meter.RateCard)
		_, err = c.SetMeter(t.Context(), key, removeCard)
		require.NoError(t, err, "removing an absent rate card is no change")
		meters, err := c.ListMeters(t.Context(), billing.PageRequest{})
		require.NoError(t, err)
		var found bool
		for _, m := range meters.Items {
			found = found || m.Key == key && m.RateCard == nil
		}
		require.True(t, found)
	}
}

// Archiving a product stops its sales and nothing else: its buyers keep their
// access and their money, and their payments still name it.
func TestArchivingAProductKeepsItsBuyers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	key, entitlement := "video-"+uuid.NewString()[:6], "video:"+uuid.NewString()[:6]
	_, err := c.ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{key: {
		DisplayName: catalog.Value("Video"), Entitlements: catalog.Value([]string{entitlement}),
		Prices: map[string]catalog.ApplyPrice{"buy": {Currency: catalog.Value("USD"), UnitAmount: catalog.Value(int64(4_000_000))}},
	}}}, billing.ApplyCatalogParams{})
	require.NoError(t, err)
	price, err := priceByKey(t.Context(), c, key, "buy")
	require.NoError(t, err)
	buyer := w.newCustomer()
	buyer.mustPurchase(embedded, order{price: price.ID, rail: "nmi", method: buyer.saveCard("nmi", visa)})

	product, err := c.UpdateProduct(t.Context(), price.ProductID, billing.UpdateProductParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	require.True(t, product.Archived)
	w.settle()

	require.True(t, buyer.entitled(entitlement), "the buyer keeps access")
	paid := completed(w.payments(embedded, buyer.id))
	require.Len(t, paid, 1)
	require.Equal(t, billing.PaymentSucceeded, paid[0].Status)
	require.Zero(t, paid[0].AmountRefunded, "nobody is refunded")
	require.NotNil(t, paid[0].OrderID)
	bought, err := c.GetOrder(t.Context(), *paid[0].OrderID)
	require.NoError(t, err)
	require.Len(t, bought.Lines, 1)
	require.Equal(t, price.ProductID, *bought.Lines[0].ProductID, "the order still names the product")
	require.Equal(t, price.ID, *bought.Lines[0].PriceID)
	kept, err := c.GetProduct(t.Context(), price.ProductID)
	require.NoError(t, err)
	require.True(t, kept.Archived, "the archived product still reads")
	for _, entry := range w.railLedger("nmi") {
		require.Zero(t, entry.Refunded)
	}
	_, err = w.newCustomer().sell(embedded, order{price: price.ID})
	require.Error(t, err, "an archived product is not for sale")
}

// A price is in fiat or SOL. A stablecoin is what a buyer pays with for a
// price in the fiat currency it tracks, so every write path refuses one.
func TestPriceCurrencyIsFiatOrSOL(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.start()
	c := w.client[remote]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "coin-" + uuid.NewString()[:8], DisplayName: "Coin", Entitlements: []string{"content:coin"}})
	require.NoError(t, err)
	for _, code := range []string{"USDC", "usdt", "PYUSD", "USD1", "USDG"} {
		_, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "coin", UnitAmount: 5_000_000, Currency: code})
		var status *billing.StatusError
		require.ErrorAs(t, err, &status, code)
		require.Equal(t, http.StatusBadRequest, status.Status, code)
		require.Contains(t, status.Message, "stablecoin", code)
	}
	var refused map[string]any
	document := map[string]any{"schema_version": 1, "products": map[string]any{product.Key: map[string]any{"prices": map[string]any{"coin": map[string]any{"amount": "5 USDC"}}}}}
	require.Equal(t, http.StatusBadRequest, w.staffCall(http.MethodPost, "/v1/catalog/apply", document, &refused), "%v", refused)
	require.Contains(t, fmt.Sprint(refused), "stablecoin")

	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "sol", UnitAmount: 500_000_000, Currency: "sol"})
	require.NoError(t, err)
	require.Equal(t, "SOL", price.Currency)
}
