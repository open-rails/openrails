package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/http/embedhttp"
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

type stubServiceCredentialResolver struct{}

func (stubServiceCredentialResolver) LooksLikeAPIKey(token string) bool { return token == "test-key" }

func (stubServiceCredentialResolver) ResolveAPIKey(context.Context, string) (*controlplane.ResolvedServiceCredential, error) {
	return &controlplane.ResolvedServiceCredential{}, nil
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

	h := s.newHTTPHandlerMux(HTTPHandlerOptions{
		RouteSets: []RouteSet{RouteSetCheckout, RouteSetCustomer, RouteSetWebhooks},
	})

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
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/v1/checkout", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Self-service /me is no longer owned by the gin-free base mux. The unified
	// self surface is mounted through embgin.SelfHandler so it can share the
	// Principal/permission gate with standalone.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/me/status", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	// Captcha discovery route (net/http handler) is reachable.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/captcha/status", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"enabled":false`)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/v1/merchants/acme/webhooks/stripe", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "merchant webhook route registered")

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

	h := s.newHTTPHandlerMux(HTTPHandlerOptions{RouteSets: []RouteSet{RouteSetMerchantAdmin}})

	// #528: the per-user `/admin` surface was removed from the base handler — the
	// admin surface is now the delegated one mounted via embgin.SelfHandler.
	// RouteSetMerchantAdmin assembles merchant support routes; mounted => 401 (not 404).
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/customers/user_123", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Merchant settings routes are a separate opt-in set.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/catalog/products", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestEmbeddedMuxPrivilegedRoutesRequireAuthenticatorAtConstruction(t *testing.T) {
	s := &Server{
		cfg:     &config.Config{},
		runtime: &app.Runtime{DB: &db.DB{}, Config: &config.Config{}},
	}

	require.PanicsWithError(t,
		"embedded billing: user/admin route groups require Options.Authenticator",
		func() { _ = s.newHTTPHandlerMux(HTTPHandlerOptions{RouteSets: []RouteSet{RouteSetMerchantAdmin}}) },
	)
	require.PanicsWithError(t,
		"embedded billing: user/admin route groups require Options.Authenticator",
		func() { _ = s.newHTTPHandlerMux(HTTPHandlerOptions{RouteSets: []RouteSet{RouteSetCustomer}}) },
	)
	require.PanicsWithError(t,
		"embedded billing: user/admin route groups require Options.Authenticator",
		func() { _ = s.newHTTPHandlerMux(HTTPHandlerOptions{RouteSets: []RouteSet{RouteSetMerchantSettings}}) },
	)
	require.NotPanics(t, func() { _ = s.newHTTPHandlerMux(HTTPHandlerOptions{RouteSets: []RouteSet{RouteSetWebhooks}}) })
}

func TestEmbeddedDefaultExcludesMerchantAPI(t *testing.T) {
	s := &Server{
		cfg:           &config.Config{Captcha: &config.CaptchaConfig{}},
		runtime:       &app.Runtime{},
		authenticator: stubAuthenticator{},
	}

	h := s.newHTTPHandlerMux(HTTPHandlerOptions{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/v1/merchant/admissions", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/catalog/products", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestEmbeddedMerchantSettingsOptInMountsSettingsRoutes(t *testing.T) {
	s := &Server{
		cfg:           &config.Config{Captcha: &config.CaptchaConfig{}},
		runtime:       &app.Runtime{},
		authenticator: stubAuthenticator{},
	}

	h := s.newHTTPHandlerMux(HTTPHandlerOptions{RouteSets: []RouteSet{RouteSetMerchantSettings}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/catalog/products", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMerchantSettingsRoutesRequireMutableCredentials(t *testing.T) {
	asm := &embedhttp.Assembler{
		Cfg:            &config.Config{Captcha: &config.CaptchaConfig{}},
		Runtime:        &app.Runtime{},
		Authenticator:  stubAuthenticator{},
		CredentialMode: embedhttp.CredentialModeFixed,
	}
	require.PanicsWithError(t,
		"embedded billing: merchant settings routes require mutable_credentials",
		func() {
			_ = asm.NewHTTPHandler(embedhttp.Options{RouteSets: []embedhttp.RouteSet{embedhttp.RouteSetMerchantSettings}})
		},
	)
}

func TestEmbeddedMerchantAPIOptInMountsServiceRoutes(t *testing.T) {
	asm := &embedhttp.Assembler{
		Cfg:                       &config.Config{Captcha: &config.CaptchaConfig{}},
		Runtime:                   &app.Runtime{},
		ServiceCredentialResolver: stubServiceCredentialResolver{},
	}
	h := asm.NewHTTPHandler(embedhttp.Options{RouteSets: []embedhttp.RouteSet{embedhttp.RouteSetMerchantAPI}})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/v1/merchant/admissions", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestEmbeddedRouteSetPresets(t *testing.T) {
	require.NotContains(t, EmbeddedDefaultRouteSets, RouteSetMerchantAPI)
	require.NotContains(t, EmbeddedDefaultRouteSets, RouteSetMerchantSettings)
	require.Contains(t, StandaloneDefaultRouteSets, RouteSetMerchantAPI)
	require.Contains(t, StandaloneDefaultRouteSets, RouteSetMerchantSettings)
}
