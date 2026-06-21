//go:build integration

package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	server "github.com/open-rails/openrails/internal/http"
)

func TestHTTPHandlerOptions_WebhooksOnly(t *testing.T) {
	srv := setupTestServer(t)
	require.NotNil(t, srv)

	h := srv.NewHTTPHandler(server.HTTPHandlerOptions{RouteSets: []server.RouteSet{server.RouteSetWebhooks}})

	// Merchant-scoped webhook route should exist; global /webhooks is not mounted.
	{
		req := httptest.NewRequest(http.MethodPost, "/billing/v1/merchants/"+dbtest.TestMerchantSlug+"/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}
	{
		req := httptest.NewRequest(http.MethodPost, "/billing/v1/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// User routes should be excluded.
	{
		req := httptest.NewRequest(http.MethodGet, "/billing/v1/products", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// Embedded handler should not accept stripped-prefix paths.
	{
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// Admin routes should be excluded.
	{
		req := httptest.NewRequest(http.MethodGet, "/billing/v1/admin/metrics/summary", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// Embedded handler must not expose standalone health endpoints.
	{
		req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}
}

func TestHTTPHandlerOptions_RouteSetPresetsOverHTTPServer(t *testing.T) {
	srv := setupTestServer(t)
	require.NotNil(t, srv)

	embeddedDefault := httptest.NewServer(srv.NewHTTPHandler(server.HTTPHandlerOptions{}))
	t.Cleanup(embeddedDefault.Close)
	require.Equal(t, http.StatusNotFound, status(t, embeddedDefault.Client(), http.MethodPost, embeddedDefault.URL+"/billing/v1/service/admit"))

	embeddedMerchantAPI := httptest.NewServer(srv.NewHTTPHandler(server.HTTPHandlerOptions{
		RouteSets: []server.RouteSet{server.RouteSetMerchantAPI},
	}))
	t.Cleanup(embeddedMerchantAPI.Close)
	require.NotEqual(t, http.StatusNotFound, status(t, embeddedMerchantAPI.Client(), http.MethodPost, embeddedMerchantAPI.URL+"/billing/v1/service/admit"))

	standalone := httptest.NewServer(srv.Handler())
	t.Cleanup(standalone.Close)
	require.NotEqual(t, http.StatusNotFound, status(t, standalone.Client(), http.MethodPost, standalone.URL+"/v1/service/admit"))
}

func status(t *testing.T, client *http.Client, method, url string) int {
	t.Helper()

	req, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}
