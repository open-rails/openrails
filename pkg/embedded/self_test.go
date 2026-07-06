package embedded

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// These tests pin the EMBEDDED mount of the host-pluggable self surface
// (#339/#467): newSelfHandler serves /billing/v1/me/* (and delegated admin)
// at the canonical embedded paths, authenticated by the host-supplied
// DelegatedAuthenticator, with the per-route permission gates intact.
// They mirror the ginroutes host-principal tests but assert the EMBEDDED
// path prefix — the contract tensorhub mounts against.

type stubSelfAuthenticator struct {
	principal *billingauth.DelegatedPrincipal
	err       error
}

func (s stubSelfAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return s.principal, s.err
}

func selfPrincipal(perms ...string) *billingauth.DelegatedPrincipal {
	return &billingauth.DelegatedPrincipal{
		MerchantID:   dbtest.TestMerchantID.String(),
		MerchantSlug: dbtest.TestMerchantSlug,
		SubjectID:    "0d4cdb35-9f3f-4a16-bb83-90a8aa20a2c1",
		Issuer:       "platform:host",
		Permissions:  perms,
	}
}

func doSelf(h http.Handler, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer host-credential")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// The self account routes answer at the CANONICAL embedded paths. rt==nil:
// reaching the handler panics (gin Recovery turns it into a 500), which proves
// auth admitted the request — a 401/403/404 would mean the surface is unmounted
// or gated wrong.
func TestSelfHandler_EmbeddedPathsMountedWithoutSelfPermissions(t *testing.T) {
	self := newSelfHandler(nil, stubSelfAuthenticator{
		principal: selfPrincipal(),
	}, nil, nil, nil)

	w := doSelf(self, http.MethodGet, "/billing/v1/me/balance")
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())

	w = doSelf(self, http.MethodGet, "/billing/v1/me/transactions")
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())

	w = doSelf(self, http.MethodPut, "/billing/v1/me/settings")
	require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())

	w = doSelf(self, http.MethodGet, "/billing/v1/merchant/subscriptions")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// Host authenticator rejection and invalid principals map to 401 (fail closed),
// exactly like the standalone host-pluggable mode.
func TestSelfHandler_FailClosed(t *testing.T) {
	h := newSelfHandler(nil, stubSelfAuthenticator{err: billingauth.ErrUnauthenticated}, nil, nil, nil)
	w := doSelf(h, http.MethodGet, "/billing/v1/me/balance?currency=usd")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	// Principal missing its explicit merchant mapping => 401.
	h = newSelfHandler(nil, stubSelfAuthenticator{
		principal: &billingauth.DelegatedPrincipal{
			SubjectID: "user-1",
		},
	}, nil, nil, nil)
	w = doSelf(h, http.MethodGet, "/billing/v1/me/balance?currency=usd")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "delegated_principal_invalid")

	// Merchant-admin routes live on the base embedded handler, not SelfHandler.
	h = newSelfHandler(nil, stubSelfAuthenticator{
		principal: selfPrincipal("platform:metrics:read"),
	}, nil, nil, nil)
	w = doSelf(h, http.MethodGet, "/billing/v1/merchant/metrics")
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

// SelfHandler (the exported constructor) fails loud without an initialized app
// graph — the identity-gated surface is never silently mounted.
func TestSelfHandler_RequiresInitializedApp(t *testing.T) {
	h, err := SelfHandler(nil, nil)
	require.Error(t, err)
	require.Nil(t, h)
}
