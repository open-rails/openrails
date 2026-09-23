package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/auth/policy"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

type fakeMerchantDelegatedResolver struct {
	resolved *controlplane.ResolvedDelegated
	err      error
}

func (f fakeMerchantDelegatedResolver) ResolveDelegated(_ *http.Request) (*controlplane.ResolvedDelegated, error) {
	return f.resolved, f.err
}

type merchantActionAuth struct{}

func (merchantActionAuth) Authenticate(_ context.Context, _ *http.Request) (billingauth.UserContext, error) {
	return billingauth.UserContext{UserID: "11111111-1111-1111-1111-111111111111", Merchant: "merchant_1"}, nil
}

type merchantActionChecker struct {
	perm    string
	allowed bool
}

func (c *merchantActionChecker) ResolveAuthorizedMerchant(_ context.Context, _, _, perm string) (merchant.ID, string, error) {
	c.perm = perm
	if !c.allowed {
		return merchant.ID{}, "", policy.ErrPermissionRequired
	}
	return merchant.ID{}, "", policy.ErrMerchantUnresolved
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
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), &app.Runtime{Config: &config.Config{AllowCatalogUpdates: true}}, opts)
	RegisterServiceRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, opts)
	RegisterCatalogRoutes(router.NewMux(mux, "/billing/v1/merchant/catalog", nil), &app.Runtime{Config: &config.Config{AllowCatalogUpdates: true}}, opts)
	RegisterMerchantConfigRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, opts)

	tests := []struct {
		name   string
		method string
		path   string
		perm   string
	}{
		{name: "billing archive export", method: http.MethodGet, path: "/billing/v1/merchant/billing-archive", perm: controlplane.PermMerchantBillingExport},
		{name: "billing archive import", method: http.MethodPost, path: "/billing/v1/merchant/billing-archive", perm: controlplane.PermMerchantBillingImport},
		{
			name:   "catalog read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/catalog/products",
			perm:   controlplane.PermMerchantCatalogRead,
		},
		{
			name:   "catalog application",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/catalog/applications",
			perm:   controlplane.PermMerchantCatalogUpdate,
		},
		{
			name:   "meter list",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/catalog/meters",
			perm:   controlplane.PermMerchantCatalogRead,
		},
		{
			name:   "meter detail",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/catalog/meters/storage-gb",
			perm:   controlplane.PermMerchantCatalogRead,
		},
		{
			name:   "meter overrides",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/catalog/meters/storage-gb/overrides",
			perm:   controlplane.PermMerchantCatalogRead,
		},
		{
			name:   "meter put",
			method: http.MethodPut,
			path:   "/billing/v1/merchant/catalog/meters/storage-gb",
			perm:   controlplane.PermMerchantCatalogUpdate,
		},
		{
			name:   "default rate card put",
			method: http.MethodPut,
			path:   "/billing/v1/merchant/catalog/meters/storage-gb/rate-card",
			perm:   controlplane.PermMerchantCatalogUpdate,
		},
		{
			name:   "default rate card delete",
			method: http.MethodDelete,
			path:   "/billing/v1/merchant/catalog/meters/storage-gb/rate-card",
			perm:   controlplane.PermMerchantCatalogUpdate,
		},
		{
			name:   "payment providers read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/payment-providers",
			perm:   controlplane.PermMerchantPaymentProvidersRead,
		},
		{
			// or#288: the routing dry run reads PSP state, so it rides the
			// payment-providers READ grant even though it is a POST.
			name:   "checkout routing dry run",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/payment-providers/routing/dry-run",
			perm:   controlplane.PermMerchantPaymentProvidersRead,
		},
		{
			// or#878 delinquency reads. Pinned here alongside the or#288 dry
			// run because the route-surface golden only proves a route EXISTS —
			// it says nothing about what guards it. A new read route silently
			// mounted ungated would pass that snapshot.
			name:   "merchant delinquency roster",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/delinquency",
			perm:   controlplane.PermMerchantCustomerSettingsRead,
		},
		{
			name:   "customer delinquency state",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/delinquency",
			perm:   controlplane.PermMerchantCustomerSettingsRead,
		},
		{
			// or#909 negotiated price overrides: merchant-scoped admin CRUD.
			name:   "rate overrides read",
			method: http.MethodGet,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/rate-overrides",
			perm:   controlplane.PermMerchantCustomerSettingsRead,
		},
		{
			name:   "rate override install",
			method: http.MethodPut,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/rate-overrides/storage.gb",
			perm:   controlplane.PermMerchantCustomerSettingsUpdate,
		},
		{
			name:   "rate override delete",
			method: http.MethodDelete,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/rate-overrides/storage.gb",
			perm:   controlplane.PermMerchantCustomerSettingsUpdate,
		},
		{
			name:   "payment providers write",
			method: http.MethodPut,
			path:   "/billing/v1/merchant/payment-providers/stripe",
			perm:   controlplane.PermMerchantPaymentProvidersUpdate,
		},
		{
			name:   "payment provider rail archive",
			method: http.MethodDelete,
			path:   "/billing/v1/merchant/payment-providers/stripe",
			perm:   controlplane.PermMerchantPaymentProvidersUpdate,
		},
		{
			// #655/#656: the explicit per-account archive is a lifecycle
			// write, gated exactly like the rail-level one.
			name:   "payment provider account archive",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/payment-providers/stripe/accounts/11111111-1111-1111-1111-111111111111/archive",
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
			name:   "replace customer spend delegations",
			method: http.MethodPut,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/spend-delegations",
			perm:   controlplane.PermMerchantCustomerSettingsUpdate,
		},
		{
			name:   "upsert customer spend delegation",
			method: http.MethodPut,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/spend-delegations:upsert",
			perm:   controlplane.PermMerchantCustomerSettingsUpdate,
		},
		{
			name:   "delete customer spend delegation",
			method: http.MethodDelete,
			path:   "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/spend-delegations/invoker/user:22222222-2222-2222-2222-222222222222",
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
			name:   "merchant subscription tier change preview",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/subscriptions/11111111-1111-1111-1111-111111111111/change-tier/preview",
			perm:   controlplane.PermMerchantSubscriptionsUpdate,
		},
		{
			name:   "merchant subscription tier change",
			method: http.MethodPost,
			path:   "/billing/v1/merchant/subscriptions/11111111-1111-1111-1111-111111111111/change-tier",
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
	tests = append(tests, struct{ name, method, path, perm string }{
		name: "merchant payment method deletion", method: http.MethodDelete,
		path: "/billing/v1/merchant/customers/11111111-1111-1111-1111-111111111111/payment-methods/22222222-2222-2222-2222-222222222222",
		perm: controlplane.PermMerchantCustomerSettingsUpdate,
	})
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
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s should not be registered on primary merchant surface, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestCatalogMeterWritesOmittedWhenDisabled(t *testing.T) {
	mux := http.NewServeMux()
	checker := &merchantActionChecker{}
	rt := &app.Runtime{Config: &config.Config{SecretBackend: config.SecretBackendSnapshot}}
	opts := Options{
		Gate: NewGate(GateOptions{
			Authenticator:          merchantActionAuth{},
			AdminPermissionChecker: checker,
		}),
	}
	RegisterCatalogRoutes(router.NewMux(mux, "/billing/v1/merchant/catalog", nil), rt, opts)

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodPut,
		"/billing/v1/merchant/catalog/meters/storage-gb",
		strings.NewReader(`{"aggregation":"count"}`),
	))
	require.Equal(t, http.StatusMethodNotAllowed, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "manifest_driven")
	require.Empty(t, checker.perm, "an omitted route cannot invoke authorization")

	recorder = httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodGet,
		"/billing/v1/merchant/catalog/meters",
		nil,
	))
	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Equal(t, controlplane.PermMerchantCatalogRead, checker.perm)
}

func TestCatalogAuthorityDoesNotChangeProviderAuthority(t *testing.T) {
	for _, merchantSource := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, allow := range []bool{false, true} {
			t.Run(merchantSource+"/"+strconv.FormatBool(allow), func(t *testing.T) {
				mux := http.NewServeMux()
				checker := &merchantActionChecker{}
				rt := &app.Runtime{Config: &config.Config{SecretBackend: merchantSource, AllowCatalogUpdates: allow}}
				opts := Options{Gate: NewGate(GateOptions{Authenticator: merchantActionAuth{}, AdminPermissionChecker: checker})}
				RegisterCatalogRoutes(router.NewMux(mux, "/catalog", nil), rt, opts)
				RegisterMerchantConfigRoutes(router.NewMux(mux, "/merchant", nil), rt, opts)
				for _, request := range []struct {
					path, permission string
					enabled          bool
				}{
					{"/catalog/meters/storage", policy.PermMerchantCatalogUpdate, allow},
					{"/merchant/payment-providers/stripe", controlplane.PermMerchantPaymentProvidersUpdate, true},
				} {
					checker.perm = ""
					rec := httptest.NewRecorder()
					mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, request.path, strings.NewReader(`{}`)))
					if !request.enabled {
						require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
						require.Empty(t, checker.perm)
					} else {
						require.Equal(t, http.StatusForbidden, rec.Code, "API authority still requires authorization")
						require.Equal(t, request.permission, checker.perm)
					}
				}
			})
		}
	}
}

// Metadata and lifecycle routes remain authorized with read-only credentials.
// Credential publication itself is refused by the selected backend.
func TestPaymentProviderArchivesMountWithoutSecretWrite(t *testing.T) {
	checker := &merchantActionChecker{}
	gate := NewGate(GateOptions{Authenticator: merchantActionAuth{}, AdminPermissionChecker: checker})
	readOnly := routesurface.AllProviderRoutes()
	readOnly.SecretWrite = false

	mux := http.NewServeMux()
	RegisterMerchantConfigRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, Options{Gate: gate, ProviderRoutes: &readOnly})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/billing/v1/merchant/payment-providers/stripe", strings.NewReader(`{}`)))
	require.Equal(t, http.StatusForbidden, rec.Code, "metadata PUT remains mounted and requires authorization")
	require.NotContains(t, rec.Body.String(), "manifest_driven")
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/billing/v1/merchant/payment-providers/stripe"},
		{http.MethodPost, "/billing/v1/merchant/payment-providers/stripe/accounts/11111111-1111-1111-1111-111111111111/archive"},
	} {
		checker.perm = ""
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s is mounted and gated", tc.method, tc.path)
		require.Equal(t, controlplane.PermMerchantPaymentProvidersUpdate, checker.perm)
	}

	manifest := http.NewServeMux()
	rt := &app.Runtime{Config: &config.Config{SecretBackend: config.SecretBackendSnapshot}}
	RegisterMerchantConfigRoutes(router.NewMux(manifest, "/billing/v1/merchant", nil), rt, Options{Gate: gate})
	checker.perm = ""
	rec = httptest.NewRecorder()
	manifest.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/billing/v1/merchant/payment-providers/stripe/accounts/11111111-1111-1111-1111-111111111111/archive", nil))
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, controlplane.PermMerchantPaymentProvidersUpdate, checker.perm, "snapshot custody does not bypass metadata authorization")
}

func TestMerchantTierChangeUsesOffChannelAdminLimit(t *testing.T) {
	mux := http.NewServeMux()
	delegated := fakeMerchantDelegatedResolver{resolved: &controlplane.ResolvedDelegated{
		DelegatedSubject: "22222222-2222-4222-8222-222222222222",
		MerchantID:       dbtest.TestMerchantID,
		Merchant:         "merchant_1",
		Permissions:      []string{controlplane.PermMerchantSubscriptionsUpdate},
	}}
	opts := Options{
		Gate:         NewGate(GateOptions{DelegatedResolver: delegated}),
		AdminLimiter: middleware.NewAdminOperationLimiter(nil),
	}
	RegisterMerchantActionRoutes(router.NewMux(mux, "/billing/v1/merchant", nil), nil, opts)

	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer aaa.bbb.ccc")
		mux.ServeHTTP(rec, req)
		return rec
	}

	previewPath := "/billing/v1/merchant/subscriptions/11111111-1111-1111-1111-111111111111/change-tier/preview"
	for range 12 {
		if rec := request(previewPath); rec.Code == http.StatusTooManyRequests {
			t.Fatal("preview must not consume the mutating tier-change limit")
		}
	}

	actionPath := "/billing/v1/merchant/subscriptions/11111111-1111-1111-1111-111111111111/change-tier"
	for range 10 {
		if rec := request(actionPath); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("tier change was limited before the off-channel allowance: %s", rec.Body.String())
		}
	}
	if rec := request(actionPath); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected tier change to use the off-channel admin limit, got %d: %s", rec.Code, rec.Body.String())
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
	RegisterCatalogRoutes(router.NewMux(mux, "/billing/v1/merchant/catalog", nil), &app.Runtime{Config: &config.Config{AllowCatalogUpdates: true}}, opts)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/billing/v1/merchant/catalog/applications", nil)
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

// Configuration metadata remains independently authorized for every credential
// backend. A read-only backend refuses secret publication inside the service;
// it must not remove metadata reads, archives or ordinary configuration updates.
func TestMerchantConfigurationRouteInventory(t *testing.T) {
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB, config.SecretBackendVault} {
		for _, writable := range []bool{false, true} {
			rt := &app.Runtime{Config: &config.Config{SecretBackend: backend}, RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable}}
			var inventory []string
			rr := router.NewMuxRecorded(http.NewServeMux(), "/merchant", rt, func(pattern string) { inventory = append(inventory, pattern) })
			RegisterMerchantConfigRoutes(rr, rt, Options{})
			for _, route := range []string{
				"GET /merchant/configuration", "POST /merchant/configuration/applications",
				"GET /merchant/settings", "PUT /merchant/settings",
				"GET /merchant/payment-providers", "PUT /merchant/payment-providers/{provider}",
				"POST /merchant/payment-providers/{provider}/accounts/{psp_id}/archive",
				"POST /merchant/webhooks", "PUT /merchant/webhooks/{id}/url",
			} {
				require.Contains(t, inventory, route)
			}
		}
	}
}

func TestOrdinaryBillingDoesNotPublishMerchantConfiguration(t *testing.T) {
	mux := http.NewServeMux()
	rt := &app.Runtime{Config: &config.Config{AllowCatalogUpdates: true}}
	opts := Options{Gate: NewGate(GateOptions{Authenticator: merchantActionAuth{}, AdminPermissionChecker: &merchantActionChecker{}})}
	RegisterServiceRoutes(router.NewMux(mux, "/merchant", rt), rt, opts)
	RegisterMerchantActionRoutes(router.NewMux(mux, "/merchant", rt), rt, opts)
	RegisterCatalogRoutes(router.NewMux(mux, "/merchant/catalog", rt), rt, opts)
	for _, path := range []string{"/merchant/configuration", "/merchant/settings", "/merchant/payment-providers", "/merchant/api-host", "/merchant/webhooks"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusNotFound, rec.Code, path)
	}
}
