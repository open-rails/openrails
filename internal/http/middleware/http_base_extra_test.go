package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSecurityHeadersHTTPSetsCSP(t *testing.T) {
	h := SecurityHeadersHTTP()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/products", nil))
	require.Equal(t, "DENY", w.Header().Get("X-Frame-Options"))
	require.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	require.Contains(t, w.Header().Get("Content-Security-Policy"), "default-src 'none'")
}

func TestCORSFromSourceHTTPGrantsOnlyListedOrigins(t *testing.T) {
	source := func(context.Context) ([]string, error) {
		return []string{"https://app.example"}, nil
	}
	h := CORSFromSourceHTTP(source)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	do := func(method, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/products", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	allowed := do(http.MethodOptions, "https://app.example")
	require.Equal(t, http.StatusNoContent, allowed.Code)
	require.Equal(t, "https://app.example", allowed.Header().Get("Access-Control-Allow-Origin"))
	require.Equal(t, "true", allowed.Header().Get("Access-Control-Allow-Credentials"))

	denied := do(http.MethodOptions, "https://evil.example")
	require.Equal(t, http.StatusNoContent, denied.Code)
	require.Empty(t, denied.Header().Get("Access-Control-Allow-Origin"))

	// Non-preflight without Origin: handler runs, no grant.
	plain := do(http.MethodGet, "")
	require.Equal(t, http.StatusOK, plain.Code)
	require.Empty(t, plain.Header().Get("Access-Control-Allow-Origin"))
}

func TestRecoverHTTPTurnsPanicInto500(t *testing.T) {
	h := RecoverHTTP()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/products", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "internal_error")
}
