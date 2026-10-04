//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
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
	req.Header.Set("OpenRails-Merchant", w.slug)
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

// public reads a buyer route with no credential.
func (w *world) public(path string) map[string]any {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodGet, w.server.URL+mountPrefix+path, nil)
	require.NoError(w.t, err)
	req.Header.Set("OpenRails-Merchant", w.slug)
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	require.Equal(w.t, http.StatusOK, res.StatusCode)
	var out map[string]any
	require.NoError(w.t, json.NewDecoder(res.Body).Decode(&out))
	return out
}

// One product and one price shape on every route that returns them: the
// merchant's, the public catalog's and the Go client's. Lists are one cursor
// envelope; a product carries its current prices.
func TestCatalogOneShapePerNoun(t *testing.T) {
	t.Parallel()
	w := prepareWorld(t, 12)
	w.mount = func(h *openrails.HTTPConfig) { h.Checkout = &openrails.CheckoutConfig{} }
	w.start()
	c := w.client[remote]
	tier := "plans-" + uuid.NewString()[:6]
	hours := monthHours
	var products []*billing.Product
	for i := range 3 {
		product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "plan-" + uuid.NewString()[:8], DisplayName: "Plan", TierGroup: &tier, TierRank: i + 1,
			EntitlementsSpec: map[string]*int{"content:plan": nil}})
		require.NoError(t, err)
		require.Empty(t, product.Prices)
		_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: int64(i+1) * 1_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
			PSPLinks: map[string]map[string]string{"nmi": {"plan_id": "plan-" + uuid.NewString()[:8]}}})
		require.NoError(t, err)
		products = append(products, product)
	}

	// The Go client and the wire agree; a product embeds its current price.
	got, err := c.GetProduct(t.Context(), products[0].ID)
	require.NoError(t, err)
	require.Len(t, got.Prices, 1)
	price := got.Prices[0]
	require.Equal(t, products[0].Key+"-monthly", price.Key, "the default key names the cadence")
	require.Equal(t, billing.PSPLinkLinked, price.PSPs["nmi"].Status)
	require.NotEmpty(t, price.PSPs["nmi"].IDs["plan_id"])
	var wire map[string]any
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodGet, "/v1/merchant/catalog/prices/"+price.ID.String(), nil, &wire))
	for _, field := range []string{"id", "key", "product_id", "archived", "unit_amount", "currency", "access_duration_hours", "auto_renew", "trial_unit_amount", "trial_duration_hours", "psps", "pending_manual_actions", "created_at", "updated_at"} {
		require.Contains(t, wire, field, "nulls are present, never omitted")
	}
	require.NotContains(t, wire, "providers")
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
	require.Equal(t, http.StatusOK, w.staffCall(http.MethodGet, "/v1/merchant/catalog/prices?limit=1&auto_renew=true", nil, &envelope))
	require.Len(t, envelope["data"], 1)
	require.NotNil(t, envelope["next_cursor"])
	require.Equal(t, http.StatusBadRequest, w.staffCall(http.MethodGet, "/v1/merchant/catalog/prices?cursor=nope", nil, nil))

	// A buyer sees the same objects, on sale only, without PSP identifiers.
	archived := true
	_, err = c.UpdateProduct(t.Context(), products[2].ID, billing.UpdateProductParams{Archived: catalog.Value(archived)})
	require.NoError(t, err)
	listed := w.public("/v1/products")
	keys := map[string]bool{}
	for _, item := range listed["data"].([]any) {
		product := item.(map[string]any)
		keys[product["key"].(string)] = true
		for _, p := range product["prices"].([]any) {
			psp := p.(map[string]any)["psps"].(map[string]any)["nmi"].(map[string]any)
			require.Equal(t, "linked", psp["status"])
			require.Nil(t, psp["ids"], "PSP identifiers stay with the merchant")
		}
	}
	require.True(t, keys[products[0].Key])
	require.False(t, keys[products[2].Key], "an archived product is not on sale")
	require.Contains(t, listed, "next_cursor")
	prices := w.public("/v1/prices?" + url.Values{"product_id": {products[0].ID.String()}, "auto_renew": {"true"}}.Encode())
	require.Len(t, prices["data"], 1)

	// Retired routes and fields are gone.
	require.Equal(t, http.StatusNotFound, w.staffCall(http.MethodPost, "/v1/merchant/catalog/products/"+products[0].ID.String()+"/activate", nil, nil))
	require.Equal(t, http.StatusNotFound, w.staffCall(http.MethodPost, "/v1/merchant/catalog/prices/"+price.ID.String()+"/key", map[string]any{"key": "x"}, nil))
	require.Equal(t, http.StatusBadRequest, w.staffCall(http.MethodPatch, "/v1/merchant/catalog/products/"+products[0].ID.String(), map[string]any{"set_tier_group": true}, nil))
}

// A product patch is a merge: omitted fields stay, null clears; a price patch
// moves the key, archiving its previous holder, and the key's history records
// both moves.
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
		second, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-b", UnitAmount: 2_000_000, Currency: "USD"})
		require.NoError(t, err)
		moved, err := c.UpdatePrice(t.Context(), second.ID, billing.UpdatePriceParams{Key: catalog.Value(first.Key)})
		require.NoError(t, err)
		require.Equal(t, first.Key, moved.Key)
		current, err := c.GetPriceByKey(t.Context(), first.Key)
		require.NoError(t, err)
		require.Equal(t, second.ID, current.ID)
		displaced, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.True(t, displaced.Archived, "the key's previous holder is archived")

		history, err := c.ListPriceKeyHistory(t.Context(), first.Key, billing.PageRequest{Limit: 1})
		require.NoError(t, err)
		require.Len(t, history.Items, 1)
		require.Equal(t, second.ID, history.Items[0].Price.ID, "most recent first")
		require.NotEmpty(t, history.Next)
		rest, err := c.ListPriceKeyHistory(t.Context(), first.Key, billing.PageRequest{Cursor: history.Next})
		require.NoError(t, err)
		require.NotEmpty(t, rest.Items)

		restored, err := c.UpdatePrice(t.Context(), first.ID, billing.UpdatePriceParams{Archived: catalog.Value(false)})
		require.NoError(t, err)
		require.False(t, restored.Archived)
	}
}

// A creator's catalog: the creator writes products and prices in its own
// catalog, created on its first write; the merchant finds it by owner; a
// creator cannot set merchant-wide fields.
func TestCreatorCatalog(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		subject := "creator/" + uuid.NewString()[:8]
		owner, err := w.client[tp].ForCatalogOwner(subject)
		require.NoError(t, err)
		product, err := owner.CreateProduct(t.Context(), billing.CreateProductParams{Key: "c-" + uuid.NewString()[:8], DisplayName: "Creator post"})
		require.NoError(t, err)
		_, err = owner.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 3_000_000, Currency: "USD"})
		require.NoError(t, err)
		_, err = owner.CreateProduct(t.Context(), billing.CreateProductParams{Key: "c-" + uuid.NewString()[:8], DisplayName: "x", EntitlementsSpec: map[string]*int{"content:x": nil}})
		requireCode(t, err, http.StatusForbidden, "catalog_owner_forbidden")

		catalogs, err := w.client[tp].ListCatalogs(t.Context(), billing.CatalogListParams{OwnerSubject: subject})
		require.NoError(t, err)
		require.Len(t, catalogs.Items, 1)
		require.Equal(t, product.CatalogID, catalogs.Items[0].ID)
		ensured, err := w.client[tp].EnsureCatalog(t.Context(), billing.EnsureCatalogParams{OwnerSubject: subject})
		require.NoError(t, err)
		require.Equal(t, product.CatalogID, ensured.ID, "ensure returns the existing catalog")
		got, err := w.client[tp].GetCatalog(t.Context(), ensured.ID)
		require.NoError(t, err)
		require.Equal(t, subject, *got.OwnerSubject)

		own, err := owner.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Len(t, own.Items, 1, "a creator sees its own catalog only")
		require.Len(t, own.Items[0].Prices, 1)
		_, err = owner.ListCatalogs(t.Context(), billing.CatalogListParams{})
		require.ErrorIs(t, err, billing.ErrDenied, "a creator client reaches its catalog only")
	}
}

// Meters, their rate card and customer overrides: one RateOverride shape
// whether listed by meter or by customer; deletes answer 204.
func TestMetersAndRateOverrides(t *testing.T) {
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
		meter, err = c.SetMeterRateCard(t.Context(), key, billing.SetMeterRateCardParams{ProductID: product.ID, Price: perUnit(10_000)})
		require.NoError(t, err)
		require.NotNil(t, meter.RateCard)
		require.Equal(t, product.ID, meter.RateCard.ProductID)

		customer, err := billing.ParseCustomerID(w.newCustomer().id)
		require.NoError(t, err)
		override, err := c.SetRateOverride(t.Context(), customer, key, billing.SetRateOverrideParams{Price: perUnit(4_000)})
		require.NoError(t, err)
		require.Equal(t, customer, override.CustomerID)
		require.EqualValues(t, 4_000, override.Price.PerUnit.UnitAmount)
		byMeter, err := c.ListMeterRateOverrides(t.Context(), key, billing.PageRequest{})
		require.NoError(t, err)
		require.Equal(t, []billing.RateOverride{*override}, byMeter.Items)
		byCustomer, err := c.ListRateOverrides(t.Context(), customer, billing.PageRequest{})
		require.NoError(t, err)
		require.Equal(t, []billing.RateOverride{*override}, byCustomer.Items)
		err = c.DeleteMeterRateCard(t.Context(), key)
		requireCode(t, err, http.StatusConflict, "rate_card_has_overrides")
		require.NoError(t, c.DeleteRateOverride(t.Context(), customer, key))
		require.NoError(t, c.DeleteMeterRateCard(t.Context(), key))
		meters, err := c.ListMeters(t.Context(), billing.PageRequest{})
		require.NoError(t, err)
		var found bool
		for _, m := range meters.Items {
			found = found || m.Key == key && m.RateCard == nil
		}
		require.True(t, found)
	}
}
