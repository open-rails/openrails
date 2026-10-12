//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

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
	sold, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sold-" + n, DisplayName: "Sold"})
	require.NoError(t, err)
	soldPrice, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sold.ID, Key: "buy", UnitAmount: 4_000_000, Currency: "USD"})
	require.NoError(t, err)
	buyer := w.newCustomer()
	buyer.mustCheckout(remote, order{price: soldPrice.ID, rail: "nmi", method: buyer.saveCard("nmi", visa)})

	require.Equal(t, http.StatusNoContent, w.staffCall(http.MethodDelete, "/v1/catalog/products/"+unsold.ID.String(), nil, nil))
	_, err = c.GetProduct(t.Context(), unsold.ID)
	requireCode(t, err, http.StatusNotFound, "product_not_found")
	_, err = productByKey(t.Context(), c, unsold.Key)
	require.ErrorIs(t, err, billing.ErrNotFound, "the listing leaves it out")
	_, err = c.GetPrice(t.Context(), unsoldPrice.ID, billing.GetPriceParams{})
	requireCode(t, err, http.StatusNotFound, "price_not_found")
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
	require.False(t, revived.Archived)
}

// A catalog file with prune: true deletes the omitted products and price keys
// only files set when nothing ever referred to them, and archives the rest,
// through POST /v1/catalog/apply and at start alike. A file without
// prune deletes nothing.
func TestPruneDeletesWhatNothingReferences(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[remote]
	price := func(amount int64) catalog.ApplyPrice {
		return catalog.ApplyPrice{Currency: catalog.Value("USD"), UnitAmount: catalog.Value(amount)}
	}
	for _, through := range []string{"apply", "start"} {
		t.Run(through, func(t *testing.T) {
			apply := func(app *catalog.Application) {
				t.Helper()
				if through == "apply" {
					_, err := c.ApplyCatalog(t.Context(), app, billing.ApplyCatalogParams{})
					require.NoError(t, err)
					return
				}
				bill, _, err := w.bootDeclared(t.Context(), app)
				require.NoError(t, err)
				require.NoError(t, bill.Close(t.Context()))
			}
			kept, unsold, sold := through+"-kept", through+"-unsold", through+"-sold"
			apply(&catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
				kept:   {DisplayName: catalog.Value("Kept"), Prices: map[string]catalog.ApplyPrice{"buy": price(1_000_000), "extra": price(2_000_000), "bought": price(3_000_000)}},
				unsold: {DisplayName: catalog.Value("Unsold"), Prices: map[string]catalog.ApplyPrice{"buy": price(1_000_000)}},
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
			buyer.mustCheckout(remote, order{price: soldPrice.ID, rail: "nmi", method: method})
			buyer.mustCheckout(remote, order{price: bought.ID, rail: "nmi", method: method})

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
		})
	}
}

// A pre-order attempt remains a catalog reference after its old price_id
// column is retired. Deleting its product cannot strand provider settlement.
func TestRetainedCheckoutEvidencePreventsCatalogDeletion(t *testing.T) {
	w := newWorld(t)
	client := w.client[remote]
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "retained-" + uuid.NewString()[:8], DisplayName: "Accepted product"})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "buy", UnitAmount: 1000000, Currency: "USD"})
	require.NoError(t, err)
	customer := w.newCustomer()
	var mid uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT merchant_id FROM billing.customers WHERE id=$1`), customer.cid().UUID()).Scan(&mid))
	// This is the exact identity envelope migration 75 adds to old accepted
	// attempts. No order, payment or provider intent supplies another reference.
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.checkout_attempts(merchant_id,id,customer_id,psp_id,rail,status,amount,currency,rail_state)
        VALUES($1,$2,$3,$4,'nmi','requires_action',1000000,'USD',jsonb_build_object('accepted_offer',jsonb_build_object('price_id',$5::uuid,'subscription',false)))`), mid, uuid.New(), customer.cid().UUID(), w.psp["nmi"].UUID(), price.ID.UUID())
	require.NoError(t, err)
	requireCode(t, client.DeleteProduct(t.Context(), product.ID), http.StatusConflict, billing.CodeProductInUse)
	_, err = client.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
}
