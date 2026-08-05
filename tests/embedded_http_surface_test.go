//go:build integration

package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestEmbeddedHandlers_Surface(t *testing.T) {
	srv := setupTestServer(t)
	require.NotNil(t, srv)

	// Full handler should include user + admin + webhooks.
	{
		req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}
	{
		req := httptest.NewRequest(http.MethodGet, "/v1/products", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}
	{
		// Merchant action route exists (will likely be 401 without auth, but must not be 404).
		req := httptest.NewRequest(http.MethodGet, "/v1/merchant/catalog/products", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}
	{
		// or#893: the merchant-slug webhook alias is NOT mounted on standalone.
		// It survives only on the embedded surface, where a pinned merchant has
		// no payload-derived identity to resolve.
		req := httptest.NewRequest(http.MethodPost, "/v1/merchants/"+dbtest.TestMerchantSlug+"/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}
	{
		// Standalone includes host-internal merchant API routes.
		req := httptest.NewRequest(http.MethodPost, "/v1/merchant/admissions", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}
	{
		// #650: the canonical provider-only global webhook surface IS mounted on
		// standalone (payload/route resolves the PSP + merchant).
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}
}
