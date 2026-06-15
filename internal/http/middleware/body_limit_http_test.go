package middleware

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func bodyLimitHandler(maxBytes int64) http.Handler {
	return BodyLimitHTTP(maxBytes)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// BodyLimitHTTP must apply the global body cap to webhook routes too. The gin
// BodyLimit middleware stopped exempting webhooks; the net/http analogue must
// match so embedded hosts get the same backstop in front of the per-processor
// caps in internal/http/handlers/webhook.go.
func TestBodyLimitHTTPAppliesToWebhookRoutes(t *testing.T) {
	for _, path := range []string{"/v1/webhooks/stripe", "/billing/v1/webhooks/stripe"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("larger-than-eight"))
		bodyLimitHandler(8).ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("path %s: status = %d, want %d (webhook routes are no longer exempt)", path, rec.Code, http.StatusRequestEntityTooLarge)
		}
	}
}

func TestBodyLimitHTTPAppliesToNonWebhookRoutes(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/checkout", strings.NewReader("larger-than-eight"))
	bodyLimitHandler(8).ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestBodyLimitHTTPAllowsWithinLimit(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/stripe", strings.NewReader("small"))
	bodyLimitHandler(1<<10).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
