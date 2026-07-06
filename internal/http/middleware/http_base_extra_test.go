package middleware

import (
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

func TestRecoverHTTPTurnsPanicInto500(t *testing.T) {
	h := RecoverHTTP()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/products", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "internal_error")
}
