package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
		MerchantID:  dbtest.TestMerchantID.String(),
		SubjectID:   "11111111-1111-1111-1111-111111111111",
		Permissions: []string{controlplane.PermSelfBillingRead},
	}, nil
}

func TestRegisterSelfServiceRoutes_MountedWithHostDelegatedAuthenticatorOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// Host authenticator set: it takes precedence, so no control plane is
	// needed to register the surface in this unit test.
	srv := &Server{cfg: &config.Config{}, delegatedAuthenticator: mountTestDelegatedAuthenticator{}}
	e := gin.New()
	srv.registerSelfServiceRoutes(e)

	// A write route the read-only principal cannot pass: 403 proves the surface
	// is MOUNTED and the host principal was ACCEPTED past authentication
	// (unmounted would 404; rejected principal would 401).
	req := httptest.NewRequest(http.MethodPost, "/v1/self/checkout", nil)
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	// The admin surface mounts alongside, gated the same way (#528: was /merchant-admin).
	req = httptest.NewRequest(http.MethodGet, "/v1/admin/subscriptions", nil)
	w = httptest.NewRecorder()
	e.ServeHTTP(w, req)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

type originRejectingDelegatedResolver struct {
	origin string
}

func (r *originRejectingDelegatedResolver) ResolveDelegated(_ context.Context, _ string, origin string) (*controlplane.ResolvedDelegated, error) {
	r.origin = origin
	return nil, controlplane.ErrDelegatedOriginNotAllowed
}

func TestRegisterSelfServiceRoutes_HTTPServerRejectsDelegatedOriginMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	resolver := &originRejectingDelegatedResolver{}
	srv := &Server{
		cfg:               &config.Config{},
		delegatedResolver: resolver,
	}
	e := srv.newPublicEngine()
	srv.registerSelfServiceRoutes(e)

	ts := httptest.NewServer(e)
	t.Cleanup(ts.Close)

	preflight, err := http.NewRequest(http.MethodOptions, ts.URL+"/v1/self/status", nil)
	require.NoError(t, err)
	preflight.Header.Set("Origin", "https://evil.example")
	preflight.Header.Set("Access-Control-Request-Method", http.MethodGet)
	preflight.Header.Set("Access-Control-Request-Headers", "authorization")

	preflightResp, err := ts.Client().Do(preflight)
	require.NoError(t, err)
	defer preflightResp.Body.Close()
	require.Equal(t, http.StatusNoContent, preflightResp.StatusCode)
	require.Equal(t, "*", preflightResp.Header.Get("Access-Control-Allow-Origin"))
	require.Empty(t, preflightResp.Header.Get("Access-Control-Allow-Credentials"))

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/self/status", nil)
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

func responseBody(t *testing.T, resp *http.Response) string {
	t.Helper()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(body)
}
