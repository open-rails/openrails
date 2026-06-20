package ginroutes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// These tests pin the HOST-PLUGGABLE identity seam for the self-service
// surface (issue #339): a host-supplied billingauth.DelegatedAuthenticator
// authenticates /v1/me/* with NO control plane wired in the test fixture
// (the host seam overrides the control-plane verifier), and the
// explicit-mapping contract is enforced fail-closed (empty merchant/subject =>
// 401; permissions outside the delegated catalog => 401).

// stubDelegatedAuthenticator is a host-style DelegatedAuthenticator returning
// a fixed principal (or error).
type stubDelegatedAuthenticator struct {
	principal *billingauth.DelegatedPrincipal
	err       error
}

func (s stubDelegatedAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return s.principal, s.err
}

func newHostPrincipalSelfRouter(t *testing.T, authn billingauth.DelegatedAuthenticator) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/me")
	// rt==nil: a request accepted past auth reaches the wrapped handler and
	// panics, which these tests recover when checking mounted self routes.
	RegisterSelfServiceRoutes(group, nil, ginmw.DelegatedPrincipalRequired(authn))
	return e
}

func hostPrincipal(perms ...string) *billingauth.DelegatedPrincipal {
	return &billingauth.DelegatedPrincipal{
		MerchantID:   dbtest.TestMerchantID.String(),
		MerchantSlug: dbtest.TestMerchantSlug,
		SubjectID:    "0d4cdb35-9f3f-4a16-bb83-90a8aa20a2c1",
		Issuer:       "https://auth.host.example",
		Permissions:  perms,
	}
}

func doHostSelf(e *gin.Engine, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer host-credential")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

// A host principal authenticates the self surface with NO control plane, and
// self routes do not require OpenRails-owned self permissions.
func TestHostPrincipal_SelfSurfaceAcceptsPrincipalWithoutControlPlane(t *testing.T) {
	e := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{
		principal: hostPrincipal(),
	})

	func() {
		defer func() { _ = recover() }()
		w := doHostSelf(e, http.MethodPost, "/v1/me/checkout")
		require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	}()
}

// EXPLICIT MAPPING — NO FALLBACKS: a principal missing its merchant or subject
// (or carrying invalid permissions) is rejected with 401 before any route
// logic runs.
func TestHostPrincipal_InvalidPrincipalsRejected(t *testing.T) {
	cases := []struct {
		name      string
		principal *billingauth.DelegatedPrincipal
		wantCode  int
	}{
		{"nil principal", nil, http.StatusUnauthorized},
		{"empty merchant", &billingauth.DelegatedPrincipal{
			SubjectID: "user-1",
		}, http.StatusUnauthorized},
		{"empty subject", &billingauth.DelegatedPrincipal{
			MerchantID: dbtest.TestMerchantID.String(),
		}, http.StatusUnauthorized},
		{"non-uuid merchant", &billingauth.DelegatedPrincipal{
			MerchantID: "tensorhub", SubjectID: "user-1",
		}, http.StatusUnauthorized},
		{"unknown grant smuggled", &billingauth.DelegatedPrincipal{
			MerchantID: dbtest.TestMerchantID.String(), SubjectID: "user-1",
			Permissions: []string{"org:not_in_browser_catalog:read"},
		}, http.StatusUnauthorized},
		{"platform grant smuggled", &billingauth.DelegatedPrincipal{
			MerchantID: dbtest.TestMerchantID.String(), SubjectID: "user-1",
			Permissions: []string{controlplane.PermPlatformSuperadmin},
		}, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{principal: tc.principal})
			w := doHostSelf(e, http.MethodGet, "/v1/me/balance")
			require.Equal(t, tc.wantCode, w.Code, w.Body.String())
			if tc.wantCode == http.StatusUnauthorized {
				require.Contains(t, w.Body.String(), "delegated_principal_invalid")
			}
		})
	}
}

// The host authenticator's own rejection maps to 401.
func TestHostPrincipal_AuthenticatorErrorIs401(t *testing.T) {
	e := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{err: billingauth.ErrUnauthenticated})
	w := doHostSelf(e, http.MethodGet, "/v1/me/balance")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

func TestHostPrincipal_AccountRoutesMountedWithoutPermissions(t *testing.T) {
	router := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{
		principal: hostPrincipal(),
	})

	func() {
		defer func() { _ = recover() }()
		w := doHostSelf(router, http.MethodPut, "/v1/me/settings")
		require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	}()

	func() {
		defer func() { _ = recover() }()
		w := doHostSelf(router, http.MethodGet, "/v1/me/transactions")
		require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	}()
}
