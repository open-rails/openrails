package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type fakeHostDelegatedAuthenticator struct {
	principal *billingauth.DelegatedPrincipal
	err       error
}

func (f fakeHostDelegatedAuthenticator) AuthenticateDelegated(_ context.Context, _ *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return f.principal, f.err
}

// TestMerchantActionRoutesHostPrincipalGated proves #565: an IN-PROCESS host
// principal (billingauth.DelegatedAuthenticator) reaches the same
// /v1/merchant/* routes as every other credential and is gated purely on
// permission. A principal lacking the route's merchant perm is denied (403); an
// auth failure is 401 — without a control plane. (The allow path runs handlers
// that need a runtime; the glob/exact match itself is unit-tested in
// controlplane.TestHasPermissionGlobUniformAcrossCredentials.)
func TestMerchantActionRoutesHostPrincipalGated(t *testing.T) {
	const customerPath = "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111"

	t.Run("missing merchant permission is denied", func(t *testing.T) {
		mux := http.NewServeMux()
		authn := fakeHostDelegatedAuthenticator{principal: &billingauth.DelegatedPrincipal{
			MerchantID:  dbtest.TestMerchantID.String(),
			SubjectID:   "11111111-1111-1111-1111-111111111111",
			Permissions: []string{"customer:spend-delegations:read"},
		}}
		RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{DelegatedAuthenticator: authn})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, customerPath, nil))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("host principal lacking a merchant perm should be 403, got %d", rec.Code)
		}
	})

	t.Run("auth failure is unauthorized", func(t *testing.T) {
		mux := http.NewServeMux()
		authn := fakeHostDelegatedAuthenticator{err: billingauth.ErrUnauthenticated}
		RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{DelegatedAuthenticator: authn})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, customerPath, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("host auth failure should be 401, got %d", rec.Code)
		}
	})
}
