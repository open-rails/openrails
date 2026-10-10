//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// The public routes find their merchant by the request's Host. A Host no
// merchant answers to is 404 merchant_not_found, never a server error; the
// merchant's api_host serves its catalog.
func TestPublicRoutesFindTheirMerchantByHost(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, nil)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	shop, err := cp.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: uniqueName("shop")})
	require.NoError(t, err)
	host := uniqueName("api") + ".e2e.test"
	require.NoError(t, cp.SetMerchantAPIHost(t.Context(), shop.MerchantID, host))

	get := func(host, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "https://"+host+path, nil))
		return w
	}
	for _, path := range []string{"/v1/catalog/products", "/v1/catalog/products?limit=1"} {
		w := get("nobody.e2e.test", path)
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		require.Equal(t, billing.CodeMerchantNotFound, errorCode(t, w))

		w = get(host, path)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	}
}
