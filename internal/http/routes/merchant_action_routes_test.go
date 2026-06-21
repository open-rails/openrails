package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/pkg/billingauth"
)

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
