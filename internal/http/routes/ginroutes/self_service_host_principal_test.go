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
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	"github.com/open-rails/openrails/pkg/billingauth"
	tenantpkg "github.com/open-rails/openrails/pkg/tenant"
)

// These tests pin the HOST-PLUGGABLE identity seam for the self-service
// surface (issue #339): a host-supplied billingauth.DelegatedAuthenticator
// authenticates /v1/self/* with NO control plane anywhere (the previously
// impossible verifier-only case), and the explicit-mapping contract is
// enforced fail-closed (empty tenant/subject/permissions => 401; permissions
// outside the delegated catalog => 401; missing write permission => 403 on
// the settings PUT).

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
	group := e.Group("/v1/self")
	// rt==nil: the wrapped handlers are never reached on the 401/403 paths
	// these tests assert; a 500/panic would mean the gate admitted the request.
	RegisterSelfServiceRoutes(group, nil, ginmw.DelegatedPrincipalRequired(authn))
	return e
}

func hostPrincipal(perms ...string) *billingauth.DelegatedPrincipal {
	return &billingauth.DelegatedPrincipal{
		TenantID:    tenantpkg.DefaultID.String(),
		TenantSlug:  tenantpkg.DefaultSlug,
		SubjectID:   "0d4cdb35-9f3f-4a16-bb83-90a8aa20a2c1",
		Actor:       "https://auth.host.example",
		Permissions: perms,
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

// A host principal authenticates the self surface with NO control plane: the
// per-route permission gate runs (403 for a missing perm — proving the route
// is mounted AND the principal was accepted past authentication).
func TestHostPrincipal_SelfSurfaceAcceptsPrincipalWithoutControlPlane(t *testing.T) {
	e := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{
		principal: hostPrincipal(controlplane.PermSelfBillingRead),
	})

	// A read-permission principal reaches past auth; the checkout gate then
	// 403s (not 401, not 404) — authentication succeeded via the host seam.
	w := doHostSelf(e, http.MethodPost, "/v1/self/checkout")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	// And a read route passes both auth and the read gate. With rt==nil the
	// handler panics if reached, which proves the gates admitted the request.
	func() {
		defer func() { _ = recover() }()
		w := doHostSelf(e, http.MethodGet, "/v1/self/account")
		require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	}()
}

// EXPLICIT MAPPING — NO FALLBACKS: a principal missing its tenant or subject
// (or carrying no/invalid permissions) is rejected with 401 before any route
// logic runs.
func TestHostPrincipal_InvalidPrincipalsRejected(t *testing.T) {
	cases := []struct {
		name      string
		principal *billingauth.DelegatedPrincipal
	}{
		{"nil principal", nil},
		{"empty tenant", &billingauth.DelegatedPrincipal{
			SubjectID: "user-1", Permissions: []string{controlplane.PermSelfBillingRead},
		}},
		{"empty subject", &billingauth.DelegatedPrincipal{
			TenantID: tenantpkg.DefaultID.String(), Permissions: []string{controlplane.PermSelfBillingRead},
		}},
		{"non-uuid tenant", &billingauth.DelegatedPrincipal{
			TenantID: "tensorhub", SubjectID: "user-1", Permissions: []string{controlplane.PermSelfBillingRead},
		}},
		{"no permissions", &billingauth.DelegatedPrincipal{
			TenantID: tenantpkg.DefaultID.String(), SubjectID: "user-1",
		}},
		{"service grant smuggled", &billingauth.DelegatedPrincipal{
			TenantID: tenantpkg.DefaultID.String(), SubjectID: "user-1",
			Permissions: []string{controlplane.PermCreditsWrite},
		}},
		{"admin grant smuggled", &billingauth.DelegatedPrincipal{
			TenantID: tenantpkg.DefaultID.String(), SubjectID: "user-1",
			Permissions: []string{controlplane.PermSelfBillingRead, controlplane.PermAdmin},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{principal: tc.principal})
			w := doHostSelf(e, http.MethodGet, "/v1/self/account")
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			require.Contains(t, w.Body.String(), "delegated_principal_invalid")
		})
	}
}

// The host authenticator's own rejection maps to 401.
func TestHostPrincipal_AuthenticatorErrorIs401(t *testing.T) {
	e := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{err: billingauth.ErrUnauthenticated})
	w := doHostSelf(e, http.MethodGet, "/v1/self/account")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// The #339 account routes are mounted and gated: a read-only principal can
// read the account but gets 403 on the settings PUT (the dedicated
// self billing:write permission is required — negative scope-separation test).
func TestHostPrincipal_AccountSettingsPutRequiresBillingWrite(t *testing.T) {
	readOnly := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{
		principal: hostPrincipal(controlplane.PermSelfBillingRead),
	})

	w := doHostSelf(readOnly, http.MethodPut, "/v1/self/account/settings")
	require.Equal(t, http.StatusForbidden, w.Code,
		"settings PUT must be mounted and write-gated (403, not 404/401): %s", w.Body.String())

	// Transactions list rides the read permission and is mounted.
	func() {
		defer func() { _ = recover() }()
		w := doHostSelf(readOnly, http.MethodGet, "/v1/self/account/transactions")
		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	}()

	// A write principal passes the settings gate (proven by the nil-runtime
	// panic guard: the gate no longer 403s).
	writer := newHostPrincipalSelfRouter(t, stubDelegatedAuthenticator{
		principal: hostPrincipal(controlplane.PermSelfBillingWrite),
	})
	func() {
		defer func() { _ = recover() }()
		w := doHostSelf(writer, http.MethodPut, "/v1/self/account/settings")
		require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
		require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	}()

	// And the write permission alone does NOT grant reads (distinct gates).
	w = doHostSelf(writer, http.MethodGet, "/v1/self/account")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}
