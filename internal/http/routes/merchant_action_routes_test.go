package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

type fakeMerchantDelegatedResolver struct {
	resolved *controlplane.ResolvedDelegated
	err      error
}

func (f fakeMerchantDelegatedResolver) ResolveDelegated(_ context.Context, _, _ string) (*controlplane.ResolvedDelegated, error) {
	return f.resolved, f.err
}

type merchantActionAuth struct{}

func (merchantActionAuth) Authenticate(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
	return billingauth.UserContext{UserID: "11111111-1111-1111-1111-111111111111", Org: "merchant_1"}, nil
}

type merchantActionChecker struct {
	perm string
}

func (c *merchantActionChecker) HasAdminPermission(_ context.Context, _, _, perm string) (bool, error) {
	c.perm = perm
	return false, nil
}

func TestRegisterMerchantActionRoutesRequiresCatalogWrite(t *testing.T) {
	mux := http.NewServeMux()
	checker := &merchantActionChecker{}
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{
		Authenticator:          merchantActionAuth{},
		AdminPermissionChecker: checker,
	})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/catalog/products", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden before handler execution, got %d", rec.Code)
	}
	if checker.perm != policy.PermMerchantCatalogUpdate {
		t.Fatalf("expected %q permission, got %q", policy.PermMerchantCatalogUpdate, checker.perm)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/billing/v1/admin/catalog/products", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("old admin catalog route should not be registered, got %d", rec.Code)
	}
}

// TestMerchantActionRoutesDelegatedTokenGated proves the #555 unified auth: a
// browser-direct delegated merchant-admin JWT is resolved on the merchant action
// surface and gated on the route's `merchant:*` permission — a token that lacks it
// is denied (403), without falling through to the user-session path.
func TestMerchantActionRoutesDelegatedTokenGated(t *testing.T) {
	mux := http.NewServeMux()
	// A delegated token that does NOT hold catalog:update.
	del := fakeMerchantDelegatedResolver{resolved: &controlplane.ResolvedDelegated{
		DelegatedSubject: "admin-1",
		Merchant:         "merchant_1",
		Permissions:      []string{controlplane.PermMerchantCustomersRead},
	}}
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{
		DelegatedResolver: del,
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/catalog/products", nil)
	req.Header.Set("Authorization", "Bearer aaa.bbb.ccc") // JWT-shaped delegated token
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delegated token lacking catalog:update should be 403, got %d", rec.Code)
	}
}
