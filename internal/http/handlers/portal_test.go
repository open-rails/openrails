package handlers

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuessBaseURLPortal(t *testing.T) {
	t.Helper()

	t.Run("prefers origin", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://billing.test/v1/stripe/portal", nil)
		req.Header.Set("Origin", "https://app.example.com")
		require.Equal(t, "https://app.example.com", guessBaseURLPortal(req))
	})

	t.Run("falls back to referer", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://billing.test/v1/stripe/portal", nil)
		req.Header.Set("Referer", "https://app.example.com/account/settings")
		require.Equal(t, "https://app.example.com", guessBaseURLPortal(req))
	})

	t.Run("uses forwarded headers", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://billing.test/v1/stripe/portal", nil)
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "api.example.com")
		require.Equal(t, "https://api.example.com", guessBaseURLPortal(req))
	})

	t.Run("uses tls host fallback", func(t *testing.T) {
		req := httptest.NewRequest("GET", "https://billing.example.com/v1/stripe/portal", nil)
		req.TLS = &tls.ConnectionState{}
		require.Equal(t, "https://billing.example.com", guessBaseURLPortal(req))
	})

	t.Run("returns empty when host missing", func(t *testing.T) {
		req := httptest.NewRequest("GET", "http://billing.test/v1/stripe/portal", nil)
		req.Host = ""
		req.Header.Del("Origin")
		req.Header.Del("Referer")
		req.Header.Del("X-Forwarded-Host")
		require.Empty(t, guessBaseURLPortal(req))
	})
}
