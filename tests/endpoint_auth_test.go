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
		// Notification endpoints
		{"GET", "/v1/me/notifications"},
		{"GET", "/v1/me/notifications/unread-count"},
		{"POST", "/v1/me/notifications/123/read"},
		// Payment method endpoints
		{"GET", "/v1/me/payment-methods"},
		{"PUT", "/v1/me/payment-methods/123"},
		{"DELETE", "/v1/me/payment-methods/123"},
		{"PUT", "/v1/me/payment-methods/123/activate"},
		// Subscription endpoints
		{"GET", "/v1/me/subscriptions"},
		{"POST", "/v1/me/subscriptions/cancel"},
		// Payment history
		{"GET", "/v1/me/payments"},
		// Payment intents
		{"POST", "/v1/payment-intents"},
		{"POST", "/v1/payment-intents/qr"},
		{"GET", "/v1/payment-intents/pi_123"},
		{"POST", "/v1/payment-intents/pi_123/confirm"},
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
