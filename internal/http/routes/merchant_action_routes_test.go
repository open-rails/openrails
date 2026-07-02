package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
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
	return billingauth.UserContext{UserID: "11111111-1111-1111-1111-111111111111", Merchant: "merchant_1"}, nil
}

type merchantActionChecker struct {
	perm string
}

func (c *merchantActionChecker) HasAdminPermission(_ context.Context, _, _, perm string) (bool, error) {
	c.perm = perm
	return false, nil
}

func TestRegisterMerchantActionRoutesPermissions(t *testing.T) {
	mux := http.NewServeMux()
	checker := &merchantActionChecker{}
	opts := Options{
		Gate: NewGate(GateOptions{
			Authenticator:          merchantActionAuth{},
			AdminPermissionChecker: checker,
		}),
	}
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, opts)
	RegisterCatalogRoutes(router.NewMux(mux, "/billing/v1/merchant/catalog", nil), nil, opts)
	RegisterPaymentProviderRoutes(router.NewMux(mux, "/billing/v1/merchant/payment-providers", nil), nil, opts)

	tests := []struct {
		name   string
		method string
		path   string
		perm   string
	}{
		{
			name:   "catalog read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/catalog/products",
			perm:   controlplane.PermMerchantCatalogRead,
		},
		{
			name:   "catalog publish",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/catalog/publish",
			perm:   controlplane.PermMerchantCatalogUpdate,
		},
		{
			name:   "payment providers read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/payment-providers",
			perm:   controlplane.PermMerchantPaymentProvidersRead,
		},
		{
			name:   "payment providers write",
			method: http.MethodPut,
			path:   "/billing/v1/merchant/payment-providers/stripe",
			perm:   controlplane.PermMerchantPaymentProvidersUpdate,
		},
		{
			name:   "customer profile",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111",
			perm:   controlplane.PermMerchantCustomerSettingsRead,
		},
		{
			name:   "customer payment methods readonly",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/payment-methods",
			perm:   controlplane.PermMerchantCustomerSettingsRead,
		},
		{
			name:   "off channel payment",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/payments/off-channel",
			perm:   controlplane.PermMerchantCustomerSettingsUpdate,
		},
		{
			name:   "merchant payments read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/payments",
			perm:   controlplane.PermMerchantPaymentsRead,
		},
		{
			name:   "merchant payment refund",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/payments/11111111-1111-1111-1111-111111111111/refunds",
			perm:   controlplane.PermMerchantPaymentsRefund,
		},
		{
			name:   "merchant subscriptions read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/subscriptions",
			perm:   controlplane.PermMerchantSubscriptionsRead,
		},
		{
			name:   "merchant subscription cancel",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/subscriptions/11111111-1111-1111-1111-111111111111/cancel",
			perm:   controlplane.PermMerchantSubscriptionsUpdate,
		},
		{
			name:   "repair alerts read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/repair-alerts",
			perm:   controlplane.PermMerchantRepairAlertsRead,
		},
		{
			name:   "worker health read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/worker-health",
			perm:   controlplane.PermMerchantRepairAlertsRead,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checker.perm = ""
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected forbidden before handler execution, got %d", rec.Code)
			}
			if checker.perm != tc.perm {
				t.Fatalf("expected %q permission, got %q", tc.perm, checker.perm)
			}
		})
	}

	for _, tc := range []struct {
		method string
		path   string
	}{
		{method: http.MethodPost, path: "/billing/v1/merchant/catalog/products/prod_123/reconcile"},
		{method: http.MethodGet, path: "/billing/v1/merchant/catalog/orphans"},
		{method: http.MethodGet, path: "/billing/v1/merchant/catalog/stripe/orphans"},
		{method: http.MethodPost, path: "/billing/v1/merchant/catalog/drift/reconcile-all"},
		{method: http.MethodGet, path: "/billing/v1/merchant/merchant-configuration"},
		{method: http.MethodPut, path: "/billing/v1/merchant/merchant-configuration"},
		{method: http.MethodDelete, path: "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/payment-methods/22222222-2222-2222-2222-222222222222"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s should not be registered on primary merchant surface, got %d", tc.method, tc.path, rec.Code)
		}
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
		Permissions:      []string{controlplane.PermMerchantCustomerSettingsRead},
	}}
	opts := Options{
		Gate: NewGate(GateOptions{DelegatedResolver: del}),
	}
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, opts)
	RegisterCatalogRoutes(router.NewMux(mux, "/billing/v1/merchant/catalog", nil), nil, opts)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/billing/v1/merchant/catalog/publish", nil)
	req.Header.Set("Authorization", "Bearer aaa.bbb.ccc") // JWT-shaped delegated token
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delegated token lacking catalog:update should be 403, got %d", rec.Code)
	}
}

func TestMerchantActionRoutesRejectCustomerTreasuryPermission(t *testing.T) {
	mux := http.NewServeMux()
	del := fakeMerchantDelegatedResolver{resolved: &controlplane.ResolvedDelegated{
		DelegatedSubject: "admin-1",
		MerchantID:       dbtest.TestMerchantID,
		Merchant:         "merchant_1",
		Permissions:      []string{controlplane.PermCustomerSpendDelegationsRead},
	}}
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{
		Gate: NewGate(GateOptions{DelegatedResolver: del}),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111", nil)
	req.Header.Set("Authorization", "Bearer aaa.bbb.ccc")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("customer treasury permission must not satisfy merchant route, got %d", rec.Code)
	}
}

func TestServiceRoutesDelegatedAdmitGatedByPermission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		perms []string
		want  int
	}{
		{name: "missing admission", perms: []string{controlplane.PermMerchantCustomerSettingsRead}, want: http.StatusForbidden},
		{name: "has admission", perms: []string{controlplane.PermMerchantAdmissionsCreate}, want: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			del := fakeMerchantDelegatedResolver{resolved: &controlplane.ResolvedDelegated{
				MerchantID:       dbtest.TestMerchantID,
				Merchant:         "merchant_1",
				DelegatedSubject: "admin-1",
				Permissions:      tc.perms,
			}}
			RegisterServiceRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{
				Gate: NewGate(GateOptions{DelegatedResolver: del}),
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/billing/v1/merchant/admissions", nil)
			req.Header.Set("Authorization", "Bearer aaa.bbb.ccc")
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d: %s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}
}
