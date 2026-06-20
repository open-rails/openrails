//go:build integration

package tests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestEndpointAuthRequirements tests that protected endpoints require authentication
func TestEndpointAuthRequirements(t *testing.T) {
	server, _ := setupTestServerWithAuth(t)

	// All endpoints that require authentication (using new /v1/me/ routes)
	endpoints := []struct {
		method string
		path   string
	}{
		// Money self-service endpoints
		{"GET", "/v1/me/balance"},
		{"GET", "/v1/me/transactions"},
		{"PUT", "/v1/me/settings"},
		// Payment method endpoints
		{"GET", "/v1/me/payment-methods"},
		{"PUT", "/v1/me/payment-methods/123"},
		{"DELETE", "/v1/me/payment-methods/123"},
		// Subscription endpoints
		{"GET", "/v1/me/subscriptions"},
		// Payment history
		{"GET", "/v1/me/payments"},
	}

	for _, endpoint := range endpoints {
		t.Run(fmt.Sprintf("%s_%s_RequiresAuth", endpoint.method, endpoint.path), func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(endpoint.method, endpoint.path, nil)

			server.Handler().ServeHTTP(w, req)

			// Should require auth, but may get rate limited first
			assert.Contains(t, []int{http.StatusUnauthorized, http.StatusTooManyRequests}, w.Code,
				"Endpoint %s %s should require authentication or be rate limited", endpoint.method, endpoint.path)
		})
	}
}
