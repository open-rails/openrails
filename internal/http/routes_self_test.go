package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// Issue #339: the self-service surface is always mounted (#469); a
// host-supplied DelegatedAuthenticator overrides the control plane's
// delegated-token verifier. This test pins the override at the
// server-registration level without a live control plane.

type mountTestDelegatedAuthenticator struct{}

func (mountTestDelegatedAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{
		MerchantID: dbtest.TestMerchantID.String(),
		SubjectID:  "11111111-1111-1111-1111-111111111111",
	}, nil
}

func TestRegisterSelfServiceRoutes_MountedWithHostDelegatedAuthenticatorOnly(t *testing.T) {
	// Host authenticator set: it takes precedence, so no control plane is
	// needed to register the surface in this unit test.
	srv := &Server{cfg: &config.Config{}, delegatedAuthenticator: mountTestDelegatedAuthenticator{}}
	mux := http.NewServeMux()
	srv.registerSelfServiceRoutes(mux)

	// A self route reaches the handler: not 401/403/404 proves the surface is
	// mounted and the host principal was accepted without self permissions.
	req := httptest.NewRequest(http.MethodPost, "/v1/me/checkout", nil)
	w := httptest.NewRecorder()
	func() {
		defer func() { _ = recover() }()
		mux.ServeHTTP(w, req)
		require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	}()
}

type originRejectingDelegatedResolver struct {
	origin string
}

func (r *originRejectingDelegatedResolver) ResolveDelegated(_ context.Context, _ string, origin string) (*controlplane.ResolvedDelegated, error) {
	r.origin = origin
	return nil, controlplane.ErrDelegatedOriginNotAllowed
}

func TestRegisterSelfServiceRoutes_HTTPServerRejectsDelegatedOriginMismatch(t *testing.T) {
	resolver := &originRejectingDelegatedResolver{}
	srv := &Server{
		cfg:               &config.Config{},
		delegatedResolver: resolver,
	}
	mux := http.NewServeMux()
	srv.registerSelfServiceRoutes(mux)
	handler := srv.wrapPublicHandler(mux)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	preflight, err := http.NewRequest(http.MethodOptions, ts.URL+"/v1/me/status", nil)
	require.NoError(t, err)
	preflight.Header.Set("Origin", "https://evil.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodGet)
	preflight.Header.Set("Access-Control-Request-Headers", "authorization")

	preflightResp, err := ts.Client().Do(preflight)
	require.NoError(t, err)
	defer preflightResp.Body.Close()
	require.Equal(t, http.StatusNoContent, preflightResp.StatusCode)
	// #765: /v1/me is browser tier — the static permissive policy grants `*`
	// regardless of Origin, but never credentials. The delegated-origin check
	// below is a SEPARATE application-layer rule (AuthKit remote_application
	// trust), unrelated to this browser CORS grant.
	require.Equal(t, "*", preflightResp.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, preflightResp.Header.Get("Access-Control-Allow-Credentials"))

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/me/status", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer delegated.jwt.token")

	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Equal(t, "https://evil.example", resolver.origin)
	require.Contains(t, responseBody(t, resp), "delegated_origin_not_allowed")
}

// TestWrapPublicHandlerPermissiveCORSOnlyOnBrowserTier proves the #765 static
// policy at the wrapPublicHandler wiring level: a pattern recorded through
// recordBrowserRoute (the browser tier — checkout/self-service/customer) gets
// the wildcard grant from ANY origin, never credentials; a plain route NOT
// recorded that way (standing in for admin/platform/merchant-API/webhooks/
// auth, none of which use recordBrowserRoute) gets no CORS headers at all.
func TestWrapPublicHandlerPermissiveCORSOnlyOnBrowserTier(t *testing.T) {
	srv := &Server{cfg: &config.Config{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /probe/browser", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv.recordBrowserRoute("GET /probe/browser")
	mux.HandleFunc("GET /probe/other", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	handler := srv.wrapPublicHandler(mux)

	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	preflight := func(path, origin string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodOptions, ts.URL+path, nil)
		require.NoError(t, err)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		req.Header.Set("Access-Control-Request-Headers", "authorization")
		resp, err := ts.Client().Do(req)
		require.NoError(t, err)
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	// Browser tier: any origin gets the static wildcard, never credentials.
	allowed := preflight("/probe/browser", "https://storefront-a.example")
	require.Equal(t, http.StatusNoContent, allowed.StatusCode)
	require.Equal(t, "*", allowed.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, allowed.Header.Get("Access-Control-Allow-Credentials"))

	alsoAllowed := preflight("/probe/browser", "https://storefront-b.example")
	require.Equal(t, http.StatusNoContent, alsoAllowed.StatusCode)
	require.Equal(t, "*", alsoAllowed.Header.Get("Access-Control-Allow-Origin"))

	// Outside the browser tier: no CORS grant at all, from any origin.
	outside := preflight("/probe/other", "https://storefront-a.example")
	require.Empty(t, outside.Header.Get("Access-Control-Allow-Origin"))

	// The actual (non-preflight) response on the browser-tier route also
	// carries the grant; outside the tier it never does.
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/probe/other", nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://storefront-a.example")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
	require.Equal(t, "ok", responseBody(t, resp))
}

func responseBody(t *testing.T, resp *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}
