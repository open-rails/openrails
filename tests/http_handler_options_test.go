//go:build integration

package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	server "github.com/open-rails/openrails/internal/http"
)

func TestHTTPHandlerOptions_WebhooksOnly(t *testing.T) {
	srv := setupTestServer(t)
	require.NotNil(t, srv)

	h := srv.NewHTTPHandler(server.HTTPHandlerOptions{IncludeWebhooks: true})

	// Webhook route should exist.
	{
		req := httptest.NewRequest(http.MethodPost, "/billing/v1/webhooks/stripe", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusNotFound, w.Code)
	}

	// User routes should be excluded.
	{
		req := httptest.NewRequest(http.MethodGet, "/billing/v1/products", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	// Embedded handler should not accept stripped-prefix legacy paths.
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
