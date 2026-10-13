//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// A product nothing ever referred to deletes with its prices: reads and
// listings leave it out, and creating its key again revives it. A product a
// purchase refers to is refused and stays.
func TestDeletingAProductNothingReferences(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[remote]
	n := uuid.NewString()[:6]
	unsold, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "unsold-" + n, DisplayName: "Unsold", Entitlements: []string{"video:unsold-" + n}})
	require.NoError(t, err)
	unsoldPrice, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: unsold.ID, Key: "buy", UnitAmount: 3_000_000, Currency: "USD"})
	require.NoError(t, err)
	require.Equal(t, unsold.Key+":buy:v1", unsoldPrice.ID.String())
	require.EqualValues(t, 1, unsoldPrice.Version)
	sold, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sold-" + n, DisplayName: "Sold"})
	require.NoError(t, err)
	soldPrice, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sold.ID, Key: "buy", UnitAmount: 4_000_000, Currency: "USD"})
	require.NoError(t, err)
	buyer := w.newCustomer()
	buyer.mustPurchase(remote, order{price: soldPrice.ID, rail: "nmi", method: buyer.saveCard("nmi", visa)})

	require.Equal(t, http.StatusNoContent, w.staffCall(http.MethodDelete, "/v1/catalog/products/"+unsold.ID.String(), nil, nil))
	_, err = c.GetProduct(t.Context(), unsold.ID)
	requireCode(t, err, http.StatusNotFound, "product_not_found")
	_, err = productByKey(t.Context(), c, unsold.Key)
	require.ErrorIs(t, err, billing.ErrNotFound, "the listing leaves it out")
	_, err = c.GetPrice(t.Context(), unsoldPrice.ID, billing.GetPriceParams{})
	requireCode(t, err, http.StatusNotFound, "price_not_found")
	namedPrices, err := c.ListPrices(t.Context(), billing.PriceListParams{IDs: []billing.PriceID{unsoldPrice.ID, soldPrice.ID}})
	require.NoError(t, err)
	require.Len(t, namedPrices.Items, 1, "canonical named-ID reads omit deleted prices")
	require.Equal(t, soldPrice.ID, namedPrices.Items[0].ID)
	namedProducts, err := c.ListProducts(t.Context(), billing.ProductListParams{IDs: []billing.ProductID{unsold.ID, sold.ID}})
	require.NoError(t, err)
	require.Len(t, namedProducts.Items, 1)
	require.Equal(t, sold.ID, namedProducts.Items[0].ID)
	listed, err := c.ListProducts(t.Context(), billing.ProductListParams{})
	require.NoError(t, err)
	for _, p := range listed.Items {
		require.NotEqual(t, unsold.ID, p.ID)
	}
	requireCode(t, c.DeleteProduct(t.Context(), unsold.ID), http.StatusNotFound, "product_not_found")

	err = c.DeleteProduct(t.Context(), sold.ID)
	requireCode(t, err, http.StatusConflict, billing.CodeProductInUse)
	kept, err := c.GetProduct(t.Context(), sold.ID)
	require.NoError(t, err)
	require.False(t, kept.Archived, "a refused delete changes nothing")

	again, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: unsold.Key, DisplayName: "Unsold again"})
	require.NoError(t, err, "the key is free again")
	require.Equal(t, unsold.ID, again.ID, "the id is the key's")
	require.Equal(t, "Unsold again", again.DisplayName)
	require.False(t, again.Archived)
	require.Empty(t, again.Entitlements, "the deleted product's keys stay closed")
	require.Empty(t, again.Prices, "its deleted prices stay deleted")
	revived, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: again.ID, Key: "buy", UnitAmount: 3_000_000, Currency: "USD"})
	require.NoError(t, err)
	require.Equal(t, unsoldPrice.ID, revived.ID, "the same terms revive the same price")
	require.EqualValues(t, 1, revived.Version)
	require.False(t, revived.Archived)
	history, err := c.ListPriceHistory(t.Context(), revived.ID, billing.PageRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, history.Items)
	require.False(t, history.Items[0].Archived, "revival records a new activation after deletion")
	require.Equal(t, revived.ID, history.Items[0].Price.ID)

	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: again.ID, Key: "buy", UnitAmount: 3_000_000, Currency: "USD"})
	require.NoError(t, err)
	replayed, err := c.ListPriceHistory(t.Context(), revived.ID, billing.PageRequest{})
	require.NoError(t, err)
	require.Equal(t, history, replayed, "identical live terms remain a no-op")

	require.NoError(t, c.DeleteProduct(t.Context(), again.ID))
	again, err = c.CreateProduct(t.Context(), billing.CreateProductParams{Key: unsold.Key, DisplayName: "Different terms"})
	require.NoError(t, err)
	repriced, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: again.ID, Key: "buy", UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	require.Equal(t, unsold.Key+":buy:v2", repriced.ID.String(), "deleted versions still reserve their immutable identity")
	require.EqualValues(t, 2, repriced.Version)
	_, err = c.GetPrice(t.Context(), unsoldPrice.ID, billing.GetPriceParams{})
	requireCode(t, err, http.StatusNotFound, "price_not_found")
}

// A catalog file with prune: true deletes the omitted products and price keys
// only files set when nothing ever referred to them, and archives the rest,
// in process and through POST /v1/catalog/apply alike. A file without prune
// deletes nothing.
func TestPruneDeletesWhatNothingReferences(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[remote]
	price := func(amount int64) catalog.ApplyPrice {
		return catalog.ApplyPrice{Currency: catalog.Value("USD"), UnitAmount: catalog.Value(amount)}
	}
	for _, through := range []topology{remote, embedded} {
		t.Run(string(through), func(t *testing.T) {
			apply := func(app *catalog.Application) {
				t.Helper()
				_, err := w.client[through].ApplyCatalog(t.Context(), app, billing.ApplyCatalogParams{})
				require.NoError(t, err)
			}
			kept, unsold, sold := string(through)+"-kept", string(through)+"-unsold", string(through)+"-sold"
			apply(&catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
				kept: {DisplayName: catalog.Value("Kept"), Prices: map[string]catalog.ApplyPrice{"buy": price(1_000_000), "extra": price(2_000_000), "bought": price(3_000_000)}},
				// Priceless, so the prune stays within one actor's destructive
				// allowance: five removed or archived objects.
				unsold: {DisplayName: catalog.Value("Unsold")},
				sold:   {DisplayName: catalog.Value("Sold"), Prices: map[string]catalog.ApplyPrice{"buy": price(1_000_000)}},
			}})
			extra, err := priceByKey(t.Context(), c, kept, "extra")
			require.NoError(t, err)
			bought, err := priceByKey(t.Context(), c, kept, "bought")
			require.NoError(t, err)
			soldPrice, err := priceByKey(t.Context(), c, sold, "buy")
			require.NoError(t, err)
			buyer := w.newCustomer()
			method := buyer.saveCard("nmi", visa)
			buyer.mustPurchase(remote, order{price: soldPrice.ID, rail: "nmi", method: method})
			buyer.mustPurchase(remote, order{price: bought.ID, rail: "nmi", method: method})

			only := &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
				kept: {DisplayName: catalog.Value("Kept"), Prices: map[string]catalog.ApplyPrice{"buy": price(1_000_000)}},
			}}
			apply(only)
			for _, key := range []string{unsold, sold} {
				_, err := productByKey(t.Context(), c, key)
				require.NoError(t, err, "a file without prune deletes nothing")
			}

			only.Prune = true
			apply(only)
			_, err = productByKey(t.Context(), c, unsold)
			require.ErrorIs(t, err, billing.ErrNotFound, "an omitted product nothing referred to is deleted")
			retired, err := productByKey(t.Context(), c, sold)
			require.NoError(t, err)
			require.True(t, retired.Archived, "an omitted product a purchase refers to is archived")
			_, err = c.GetPrice(t.Context(), extra.ID, billing.GetPriceParams{})
			requireCode(t, err, http.StatusNotFound, "price_not_found")
			archived, err := c.GetPrice(t.Context(), bought.ID, billing.GetPriceParams{})
			require.NoError(t, err)
			require.True(t, archived.Archived, "an omitted price a purchase refers to is archived")
			live, err := priceByKey(t.Context(), c, kept, "buy")
			require.NoError(t, err)
			require.False(t, live.Archived)

			// The prune spent the staff member's hourly allowance.
			w.advance(2 * time.Hour)
			only.Products[kept] = catalog.ApplyProduct{DisplayName: catalog.Value("Kept"), Prices: map[string]catalog.ApplyPrice{"buy": price(1_000_000), "extra": price(2_000_000)}}
			apply(only)
			revived, err := priceByKey(t.Context(), c, kept, "extra")
			require.NoError(t, err)
			require.Equal(t, extra.ID, revived.ID)
			require.EqualValues(t, 1, revived.Version)
			history, err := c.ListPriceHistory(t.Context(), revived.ID, billing.PageRequest{})
			require.NoError(t, err)
			require.NotEmpty(t, history.Items)
			require.False(t, history.Items[0].Archived, "redeclaring a pruned key records its activation")
		})
	}
}
