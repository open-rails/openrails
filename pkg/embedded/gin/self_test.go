package gin

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
// (#339/#467): newSelfHandler serves /billing/v1/self/* (and merchant-admin)
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

// The #339 account routes answer at the CANONICAL embedded paths. rt==nil:
// reaching the handler panics (gin Recovery turns it into a 500), which proves
// the auth + permission gates ADMITTED the request — a 401/403/404 would mean
// the surface is unmounted or gated wrong.
func TestSelfHandler_EmbeddedPathsMountedAndGated(t *testing.T) {
	readOnly := newSelfHandler(nil, nil, stubSelfAuthenticator{
		principal: selfPrincipal("openrails:self:billing:read"),
	})

	// Read route past both gates (500 = nil-runtime handler reached).
	w := doSelf(readOnly, http.MethodGet, "/billing/v1/self/account")
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())

	// Transactions list rides the read permission.
	w = doSelf(readOnly, http.MethodGet, "/billing/v1/self/account/transactions")
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())

	// Settings PUT is mounted but write-gated: read-only principal => 403.
	w = doSelf(readOnly, http.MethodPut, "/billing/v1/self/account/settings")
	require.Equal(t, http.StatusForbidden, w.Code,
		"settings PUT must be mounted and write-gated (403, not 404/401): %s", w.Body.String())

	// A write principal passes the settings gate.
	writer := newSelfHandler(nil, nil, stubSelfAuthenticator{
		principal: selfPrincipal("openrails:self:billing:write"),
	})
	w = doSelf(writer, http.MethodPut, "/billing/v1/self/account/settings")
	require.NotEqual(t, http.StatusForbidden, w.Code, w.Body.String())
	require.NotEqual(t, http.StatusNotFound, w.Code, w.Body.String())

	// The merchant-admin subtree is mounted on the same handler (read perm
	// missing => 403 proves mount + auth, not 404).
	w = doSelf(readOnly, http.MethodGet, "/billing/v1/merchant-admin/subscriptions")
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
}

// Host authenticator rejection and invalid principals map to 401 (fail closed),
// exactly like the standalone host-pluggable mode.
func TestSelfHandler_FailClosed(t *testing.T) {
	h := newSelfHandler(nil, nil, stubSelfAuthenticator{err: billingauth.ErrUnauthenticated})
	w := doSelf(h, http.MethodGet, "/billing/v1/self/account")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	// Principal missing its explicit tenant mapping => 401.
	h = newSelfHandler(nil, nil, stubSelfAuthenticator{
		principal: &billingauth.DelegatedPrincipal{
			SubjectID:   "user-1",
			Permissions: []string{"openrails:self:billing:read"},
		},
	})
	w = doSelf(h, http.MethodGet, "/billing/v1/self/account")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "delegated_principal_invalid")

	// A service/operator grant can never be smuggled onto the self surface.
	h = newSelfHandler(nil, nil, stubSelfAuthenticator{
		principal: selfPrincipal("openrails:credits:write"),
	})
	w = doSelf(h, http.MethodGet, "/billing/v1/self/account")
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
}

// SelfHandler (the exported constructor) fails loud without an initialized app
// graph — the identity-gated surface is never silently mounted.
func TestSelfHandler_RequiresInitializedApp(t *testing.T) {
	h, err := SelfHandler(nil)
	require.Error(t, err)
	require.Nil(t, h)
}
