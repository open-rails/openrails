//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

// A product read is sufficient to render its current prices; each price can
// also be read independently, and retiring one preserves its historical lookup.
func TestProductReadsIncludeCurrentPrices(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "lookup-" + uuid.NewString(), DisplayName: "Lookup"})
	require.NoError(t, err)
	monthly, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: new(720), AccessDurationHours: new(720)})
	require.NoError(t, err)
	once, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "once", UnitAmount: 90_000_000, Currency: "USD"})
	require.NoError(t, err)
	for _, topology := range []topology{embedded, remote} {
		client := w.client[topology]
		listed, err := client.ListProducts(t.Context(), billing.ProductListParams{IDs: []billing.ProductID{product.ID}})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		got, err := client.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		for _, read := range []billing.Product{listed.Items[0], *got} {
			require.ElementsMatch(t, []billing.Price{*monthly, *once}, read.Prices, topology)
			for _, nested := range read.Prices {
				price, err := client.GetPrice(t.Context(), nested.ID, billing.GetPriceParams{})
				require.NoError(t, err)
				require.Equal(t, nested, *price)
			}
		}
	}
	_, err = c.UpdatePrice(t.Context(), once.ID, billing.UpdatePriceParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	for _, topology := range []topology{embedded, remote} {
		client := w.client[topology]
		got, err := client.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.Len(t, got.Prices, 1)
		require.Equal(t, monthly.ID, got.Prices[0].ID)
		retired, err := client.GetPrice(t.Context(), once.ID, billing.GetPriceParams{})
		require.NoError(t, err)
		require.True(t, retired.Archived)
		prices, err := client.ListPrices(t.Context(), billing.PriceListParams{ProductID: product.ID, Archived: new(true)})
		require.NoError(t, err)
		require.Len(t, prices.Items, 1)
		require.Equal(t, once.ID, prices.Items[0].ID)
	}
}

func TestCatalogReaderCannotEditProducts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	token := w.auth.issue(t, grant{subject: "catalog-reader", role: "only:" + string(billing.RootCatalogRead), kind: string(openrails.SubjectApplication), credential: string(openrails.CredentialAPIKey)})
	status, body := w.merchantJSON(token, http.MethodGet, "/v1/catalog/products", nil)
	require.Equal(t, http.StatusOK, status, body)
	status, body = w.merchantJSON(token, http.MethodPost, "/v1/catalog/products", map[string]any{"key": "forbidden", "display_name": "Forbidden"})
	require.Equal(t, http.StatusForbidden, status, body)
	require.Equal(t, billing.CodePermissionRequired, body["error"].(map[string]any)["code"])
}
