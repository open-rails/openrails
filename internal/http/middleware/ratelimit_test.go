package middleware

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClassifyBucketMatchesRegisteredRoutes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		path   string
		method string
		want   string
	}{
		{name: "webhook", path: "/v1/webhooks/stripe", method: http.MethodPost, want: "webhook"},
		{name: "embedded webhook", path: "/billing/v1/webhooks/mobius", method: http.MethodPost, want: "webhook"},
		{name: "checkout create", path: "/v1/checkout", method: http.MethodPost, want: "checkout"},
		{name: "checkout confirm", path: "/v1/checkout/checkout_123/confirm", method: http.MethodPost, want: "checkout"},
		{name: "payment method create", path: "/v1/me/payment-methods", method: http.MethodPost, want: "payment-methods"},
		{name: "payment method update", path: "/billing/v1/me/payment-methods/pm_123", method: http.MethodPut, want: "payment-methods"},
		{name: "subscription cancel", path: "/v1/me/subscriptions/sub_123/cancel", method: http.MethodPost, want: "subscriptions"},
		{name: "subscription payment method update", path: "/billing/v1/me/subscriptions/sub_123/payment-method", method: http.MethodPut, want: "subscriptions"},
		{name: "subscription read default", path: "/v1/me/subscriptions/sub_123", method: http.MethodGet, want: "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, classifyBucket(tt.path, tt.method))
		})
	}
}
