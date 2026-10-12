//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A request's Host names no merchant: at a host spelled after merchant A,
// B's API key reads B, no credential is refused, and the API host and public
// catalog routes are gone.
func TestMerchantHostNamesNothing(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	ctx := t.Context()
	shop := func(name string) (billing.MerchantID, string) {
		slug := uniqueName(name)
		mid := provision(t, srv, slug)
		product, err := handle(t, srv, mid).CreateProduct(ctx, billing.CreateProductParams{Key: name, DisplayName: name})
		require.NoError(t, err)
		_, err = handle(t, srv, mid).CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "once", Currency: "USD", UnitAmount: 5_000_000})
		require.NoError(t, err)
		return mid, slug
	}
	_, a := shop("host-a")
	b, _ := shop("host-b")
	key := "Bearer " + merchantKey(t, srv, b, "owner").Secret

	get := func(path, authorization string) *httptest.ResponseRecorder {
		return serve(handler, rsRequest{path: "https://" + a + ".api.billing.example" + path, authorization: authorization})
	}
	keys := func(w *httptest.ResponseRecorder) []string {
		t.Helper()
		var out []string
		for _, p := range productsOf(t, w) {
			out = append(out, p.Key)
		}
		return out
	}
	require.Equal(t, []string{"host-b"}, keys(get("/v1/catalog/products", key)), "the key's merchant, not the Host's")
	require.Equal(t, http.StatusOK, get("/v1/admin/customers", key).Code)
	public := "/v1/products?" + url.Values{"merchant": {b.String()}, "keys": {"host-a", "host-b"}}.Encode()
	require.Equal(t, http.StatusNotFound, get(public, "").Code, "no public catalog")

	w := get("/v1/admin/customers", "")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	w = get("/v1/admin/api-host", key)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}
