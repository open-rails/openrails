package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// stubAuthenticator authenticates iff the Authorization header equals "Bearer ok".
type stubAuthenticator struct{}

func (stubAuthenticator) Authenticate(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
	if r.Header.Get("Authorization") == "Bearer ok" {
		return billingauth.UserContext{UserID: "user_1"}, nil
	}
	return billingauth.UserContext{}, billingauth.ErrUnauthenticated
}

// TestEmbeddedMuxAssembles ensures the gin-free embedded ServeMux assembly does
// not panic on ServeMux pattern conflicts and routes through the neutral
// middleware chain end-to-end (issue #282). It mounts user + webhook groups (no
// DB so the tenant-conn middleware is skipped) and exercises optional auth,
// required auth (401), and a path param.
func TestEmbeddedMuxAssembles(t *testing.T) {
	s := &Server{
		cfg:           &config.Config{Captcha: &config.CaptchaConfig{}},
		runtime:       &app.Runtime{},
		authenticator: stubAuthenticator{},
	}

	h := s.newHTTPHandlerMux(HTTPHandlerOptions{IncludeUser: true, IncludeWebhooks: true})

	// Optional-auth public route: reaches the handler even unauthenticated.
	// (GetSupportedTokens has no auth and no DB dependency.)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/solana/tokens", nil))
	require.NotEqual(t, http.StatusNotFound, rec.Code, "route not registered")
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, "public route should not require auth")

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/solana/config", nil))
	require.NotEqual(t, http.StatusNotFound, rec.Code, "route not registered")
	require.NotEqual(t, http.StatusUnauthorized, rec.Code, "public route should not require auth")

	// Required-auth route without a credential -> 401 from the neutral requiredMW.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/me/status", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Captcha discovery route (net/http handler) is reachable.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/captcha/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"enabled":false`)

	// Security headers applied by the base middleware stack.
	require.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
}

// TestEmbeddedMuxAdminAssembles ensures the admin group's ~47 routes register on
// the ServeMux without a pattern conflict and that an unauthenticated admin
// request is rejected by the neutral required-auth gate before any DB access.
func TestEmbeddedMuxAdminAssembles(t *testing.T) {
	s := &Server{
		cfg:           &config.Config{},
		runtime:       &app.Runtime{DB: &db.DB{}, Config: &config.Config{}},
		authenticator: stubAuthenticator{},
	}

	h := s.newHTTPHandlerMux(HTTPHandlerOptions{IncludeAdmin: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/admin/subscriptions", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/catalog/products", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/admin/catalog/products", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}
