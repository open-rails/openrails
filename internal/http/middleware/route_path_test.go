package middleware

import (
	"net/http/httptest"
	"testing"
)

func TestLoggedPathHidesTheWebhookToken(t *testing.T) {
	for path, want := range map[string]string{
		"/v1/webhooks/ccbill/999999-0000/secret-token-value":        "/v1/webhooks/ccbill/999999-0000/{token}",
		"/billing/v1/webhooks/ccbill/999999-0000/secret-token/more": "/billing/v1/webhooks/ccbill/999999-0000/{token}",
		"/v1/webhooks/ccbill/999999-0000":                           "/v1/webhooks/ccbill/999999-0000",
		"/v1/me/invoices":                                           "/v1/me/invoices",
	} {
		if got := loggedPath(httptest.NewRequest("POST", path, nil)); got != want {
			t.Errorf("loggedPath(%q) = %q, want %q", path, got, want)
		}
	}
}
