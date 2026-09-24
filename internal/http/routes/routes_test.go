package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// deny records every permission asked of it and refuses.
type deny struct{ asked []string }

func (g *deny) Authorize(_ context.Context, _ *http.Request, perm string) (billingauth.Principal, error) {
	g.asked = append(g.asked, perm)
	return billingauth.Principal{}, billingauth.GateError{Status: http.StatusForbidden, Message: "permission_required"}
}

var wildcard = regexp.MustCompile(`\{[^}]+\}`)

func routeKeys(table *router.Table) []string {
	var keys []string
	for _, e := range table.Entries {
		keys = append(keys, e.Method+" "+e.Path)
	}
	return keys
}

func do(h http.Handler, method, path string, header map[string]string) *httptest.ResponseRecorder {
	return doBody(h, method, path, "{}", header)
}

func doBody(h http.Handler, method, path, body string, header map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		r.Header.Set(k, v)
	}
	h.ServeHTTP(rec, r)
	return rec
}

// merchantSurface mounts every merchant-authorized registration the way the
// standalone server does.
func merchantSurface(rt *app.Runtime, opts Options) *router.Table {
	table := &router.Table{}
	RegisterServiceRoutes(router.NewMux(table, "/v1/merchant", rt), rt, opts)
	RegisterMerchantActionRoutes(router.NewMux(table, "/v1/merchant", rt), rt, opts)
	RegisterMerchantConfigRoutes(router.NewMux(table, "/v1/merchant", rt), rt, opts)
	RegisterImportRoutes(router.NewMux(table, "/v1/import", rt), rt, opts)
	RegisterCatalogRoutes(router.NewMux(table, "/v1/merchant/catalog", rt), rt, opts)
	RegisterCatalogCollectionRoutes(router.NewMux(table, "/v1/merchant/catalogs", rt), rt, opts)
	RegisterOwnedCatalogRoutes(router.NewMux(table, "/v1/catalog", rt), rt, opts)
	return table
}

// Every merchant route asks the gate before its handler, and asks for the
// permission that matches its blast radius.
func TestMerchantRouteAuthorization(t *testing.T) {
	gate := &deny{}
	rt := &app.Runtime{Config: &config.Config{AllowCatalogUpdates: true}}
	table := merchantSurface(rt, Options{Gate: gate})
	h := table.Handler()
	asked := map[string]string{}
	for _, key := range routeKeys(table) {
		method, path, _ := strings.Cut(key, " ")
		gate.asked = nil
		rec := do(h, method, wildcard.ReplaceAllString(path, "x"), nil)
		require.Equal(t, http.StatusForbidden, rec.Code, key)
		require.NotEmpty(t, gate.asked, "%s reached its handler without authorization", key)
		asked[key] = gate.asked[0]
	}

	p := permissions.MerchantCustomerSettingsRead
	w := permissions.MerchantCustomerSettingsUpdate
	for key, perm := range map[string]string{
		"GET /v1/merchant/billing-archive":                                                  permissions.MerchantBillingExport,
		"POST /v1/merchant/billing-archive":                                                 permissions.MerchantBillingImport,
		"POST /v1/import/billing":                                                           permissions.MerchantBillingImport,
		"GET /v1/merchant/host-events":                                                      permissions.MerchantHostEventsRead,
		"POST /v1/merchant/host-events/{id}/acknowledge":                                    permissions.MerchantHostEventsAcknowledge,
		"POST /v1/merchant/customers/entitlements:batch":                                    p,
		"PUT /v1/merchant/customers/{customer_id}":                                          w,
		"GET /v1/merchant/customers/{customer_id}":                                          p,
		"GET /v1/merchant/customers/{customer_id}/delinquency":                              p,
		"GET /v1/merchant/delinquency":                                                      p,
		"GET /v1/merchant/customers/{customer_id}/payment-settlement-status":                permissions.MerchantPaymentsRead,
		"PUT /v1/merchant/customers/{customer_id}/spend-delegations:upsert":                 w,
		"DELETE /v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}": w,
		"DELETE /v1/merchant/customers/{customer_id}/payment-methods/{id}":                  w,
		"POST /v1/merchant/customers/{customer_id}/payments/off-channel":                    w,
		"POST /v1/merchant/customers/{customer_id}/entitlements":                            w,
		"PUT /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}":               w,
		"PUT /v1/merchant/customers/{customer_id}/invoice-profile":                          w,
		"POST /v1/merchant/customers/{customer_id}/credits":                                 permissions.MerchantCreditsGrant,
		"DELETE /v1/merchant/customers/{customer_id}/credits/{grant_id}":                    permissions.MerchantCreditsRevoke,
		"POST /v1/merchant/credits/deposit":                                                 w,
		"GET /v1/merchant/credits/deposit":                                                  p,
		"PUT /v1/merchant/credit-limit":                                                     w,
		"POST /v1/merchant/checkout-sessions":                                               permissions.MerchantCheckoutCreate,
		"GET /v1/merchant/checkout-sessions/{id}":                                           p,
		"POST /v1/merchant/admissions":                                                      permissions.MerchantAdmissionsCreate,
		"POST /v1/merchant/admissions/{id}/capture":                                         permissions.MerchantAdmissionsCreate,
		"POST /v1/merchant/provider-operations":                                             permissions.MerchantAdmissionsCreate,
		"GET /v1/merchant/provider-operations/{operation_id}":                               permissions.MerchantUsageRead,
		"POST /v1/merchant/usage/report":                                                    permissions.MerchantAdmissionsCreate,
		"POST /v1/merchant/usage/rollup":                                                    permissions.MerchantUsageRead,
		"GET /v1/merchant/payments":                                                         permissions.MerchantPaymentsRead,
		"POST /v1/merchant/payments/{id}/refunds":                                           permissions.MerchantPaymentsRefund,
		"POST /v1/merchant/purchase-reviews/{id}/resolve":                                   permissions.MerchantPaymentsRefund,
		"GET /v1/merchant/subscriptions":                                                    permissions.MerchantSubscriptionsRead,
		"POST /v1/merchant/subscriptions/{id}/cancel":                                       permissions.MerchantSubscriptionsUpdate,
		"POST /v1/merchant/subscriptions/{id}/change-tier":                                  permissions.MerchantSubscriptionsUpdate,
		"POST /v1/merchant/subscriptions/{id}/engine-takeover/preview":                      permissions.MerchantSubscriptionsRead,
		"POST /v1/merchant/engine-takeovers":                                                permissions.MerchantSubscriptionsUpdate,
		"POST /v1/merchant/catalog/reprice-all-prior-versions":                              permissions.MerchantSubscriptionsUpdate,
		"GET /v1/merchant/invoices":                                                         permissions.MerchantInvoicesRead,
		"POST /v1/merchant/invoices/{id}/void":                                              permissions.MerchantInvoicesUpdate,
		"POST /v1/merchant/invoices/{id}/payments":                                          permissions.MerchantInvoicesUpdate,
		"POST /v1/merchant/invoices/{id}/retry-collection":                                  permissions.MerchantInvoicesCollect,
		"POST /v1/merchant/metrics/query":                                                   permissions.MerchantMetricsRead,
		"PUT /v1/merchant/dashboard":                                                        permissions.MerchantDashboardUpdate,
		"GET /v1/merchant/notifications":                                                    permissions.MerchantMetricsRead,
		"POST /v1/merchant/notifications/{id}/read":                                         permissions.MerchantSettingsUpdate,
		"GET /v1/merchant/repair-alerts":                                                    permissions.MerchantRepairAlertsRead,
		"GET /v1/merchant/worker-health":                                                    permissions.MerchantRepairAlertsRead,
		"GET /v1/merchant/findings/{id}":                                                    permissions.MerchantRepairAlertsRead,
		"POST /v1/merchant/findings/{id}/resolve":                                           permissions.MerchantFindingsResolve,
		"GET /v1/merchant/configuration":                                                    permissions.MerchantSettingsRead,
		"POST /v1/merchant/configuration/applications":                                      permissions.MerchantSettingsUpdate,
		"PUT /v1/merchant/webhooks/{id}/url":                                                permissions.MerchantSettingsUpdate,
		"GET /v1/merchant/payment-providers":                                                permissions.MerchantPaymentProvidersRead,
		"POST /v1/merchant/payment-providers/routing/dry-run":                               permissions.MerchantPaymentProvidersRead,
		"PUT /v1/merchant/payment-providers/{provider}":                                     permissions.MerchantPaymentProvidersUpdate,
		"DELETE /v1/merchant/payment-providers/{provider}":                                  permissions.MerchantPaymentProvidersUpdate,
		"POST /v1/merchant/payment-providers/{provider}/accounts/{psp_id}/archive":          permissions.MerchantPaymentProvidersUpdate,
		"GET /v1/merchant/catalog/products":                                                 permissions.MerchantCatalogRead,
		"POST /v1/merchant/catalog/offers/lookup":                                           permissions.MerchantCatalogRead,
		"POST /v1/merchant/catalog/applications":                                            permissions.MerchantCatalogUpdate,
		"PUT /v1/merchant/catalog/meters/{key}":                                             permissions.MerchantCatalogUpdate,
		"DELETE /v1/merchant/catalog/meters/{key}/rate-card":                                permissions.MerchantCatalogUpdate,
		"POST /v1/merchant/catalog/product-archives":                                        permissions.MerchantCatalogUpdate,
		"POST /v1/merchant/catalogs":                                                        permissions.MerchantCatalogUpdate,
		"GET /v1/catalog":                                                                   permissions.MerchantCatalogOwnRead,
		"PUT /v1/catalog":                                                                   permissions.MerchantCatalogOwnUpdate,
		"POST /v1/catalog/offers/lookup":                                                    permissions.MerchantCatalogOwnRead,
		"POST /v1/catalog/products":                                                         permissions.MerchantCatalogOwnUpdate,
	} {
		require.Contains(t, asked, key)
		require.Equal(t, perm, asked[key], key)
	}

	// Money-moving catalog archives need both catalog and refund authority.
	gate.asked = nil
	allowCatalog := gateFunc(func(ctx context.Context, r *http.Request, perm string) (billingauth.Principal, error) {
		if perm == permissions.MerchantCatalogUpdate {
			return billingauth.Principal{MerchantID: merchantA}, nil
		}
		return gate.Authorize(ctx, r, perm)
	})
	rec := do(merchantSurface(rt, Options{Gate: allowCatalog}).Handler(), http.MethodPost, "/v1/merchant/catalog/product-archives", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, []string{permissions.MerchantPaymentsRefund}, gate.asked)

	// Retired and control-plane-only management routes are not mounted.
	for _, key := range routeKeys(table) {
		for _, retired := range []string{"/api-keys", "/team", "/orphans", "/reconcile", "/merchant-configuration", "/api-host"} {
			require.NotContains(t, key, retired)
		}
	}
}

// Ordinary billing surfaces never publish merchant configuration; it exists
// only where RegisterMerchantConfigRoutes is explicitly mounted.
func TestConfigurationIsSeparatelyMounted(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{AllowCatalogUpdates: true}}
	table := &router.Table{}
	RegisterServiceRoutes(router.NewMux(table, "/m", rt), rt, Options{})
	RegisterMerchantActionRoutes(router.NewMux(table, "/m", rt), rt, Options{})
	RegisterCatalogRoutes(router.NewMux(table, "/m/catalog", rt), rt, Options{})
	for _, key := range routeKeys(table) {
		for _, forbidden := range []string{" /m/configuration", " /m/settings", " /m/payment-providers", " /m/webhooks"} {
			require.NotContains(t, key, forbidden)
		}
	}

	// Provider metadata and lifecycle archives stay mounted (and gated) even
	// when the secret backend is read-only.
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB, config.SecretBackendVault} {
		for _, writable := range []bool{false, true} {
			rt := &app.Runtime{Config: &config.Config{SecretBackend: backend}, RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable}}
			table := &router.Table{}
			RegisterMerchantConfigRoutes(router.NewMux(table, "/m", rt), rt, Options{})
			keys := routeKeys(table)
			for _, key := range []string{
				"GET /m/configuration", "POST /m/configuration/applications", "GET /m/settings", "PUT /m/settings",
				"GET /m/payment-providers", "PUT /m/payment-providers/{provider}", "DELETE /m/payment-providers/{provider}",
				"POST /m/payment-providers/{provider}/accounts/{psp_id}/archive", "POST /m/webhooks", "PUT /m/webhooks/{id}/url",
			} {
				require.Contains(t, keys, key, "%s/%v", backend, writable)
			}
		}
	}
}

// Disabled catalog updates remove every catalog mutation registration — not
// merely reject it — while reads, batch lookups and credit grants remain.
func TestCatalogWritePolicy(t *testing.T) {
	reads := []string{"POST /merchant/catalog/offers/lookup", "POST /catalog/offers/lookup"}
	for _, allow := range []bool{false, true} {
		rt := &app.Runtime{Config: &config.Config{AllowCatalogUpdates: allow}}
		table := &router.Table{}
		RegisterCatalogRoutes(router.NewMux(table, "/merchant/catalog", rt), rt, Options{})
		RegisterOwnedCatalogRoutes(router.NewMux(table, "/catalog", rt), rt, Options{})
		RegisterCatalogCollectionRoutes(router.NewMux(table, "/merchant/catalogs", rt), rt, Options{})
		keys := routeKeys(table)
		for _, key := range append([]string{"GET /merchant/catalog/revision", "GET /merchant/catalog/meters", "GET /merchant/catalog/product-archives/{id}", "GET /catalog", "GET /catalog/products", "GET /merchant/catalogs"}, reads...) {
			require.Contains(t, keys, key)
		}
		mutations := 0
		for _, key := range keys {
			if !strings.HasPrefix(key, "GET ") && !slices.Contains(reads, key) {
				mutations++
				require.True(t, allow, "disabled catalog mutation registered: %s", key)
			}
		}
		require.Equal(t, allow, mutations > 0)
		if allow {
			for _, key := range []string{"POST /merchant/catalog/applications", "PUT /merchant/catalog/products/by-key/{key}", "POST /merchant/catalog/prices/{id}/key", "DELETE /merchant/catalog/meters/{key}/rate-card", "POST /merchant/catalog/product-archives", "PUT /catalog", "POST /catalog/prices", "POST /merchant/catalogs"} {
				require.Contains(t, keys, key)
			}
		}

		action := &router.Table{}
		RegisterMerchantActionRoutes(router.NewMux(action, "/merchant", rt), rt, Options{})
		keys = routeKeys(action)
		require.Contains(t, keys, "GET /merchant/customers/{customer_id}/rate-overrides")
		require.Contains(t, keys, "POST /merchant/customers/{customer_id}/credits", "credit grants are not catalog authoring")
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			require.Equal(t, allow, slices.Contains(keys, method+" /merchant/customers/{customer_id}/rate-overrides/{meter_key}"))
		}
	}

	called := false
	rec := httptest.NewRecorder()
	catalogWriteGuardMW(&config.Config{})(func(*httprequest.Request) { called = true })(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/products", nil), nil))
	require.Equal(t, http.StatusForbidden, rec.Code, "the guard also refuses if a write is ever mounted")
	require.Contains(t, rec.Body.String(), "catalog_updates_disabled")
	require.False(t, called)
}

// Administrative velocity limits apply per operation class after
// authorization; previews never consume the mutation allowance.
func TestAdminOperationLimits(t *testing.T) {
	allow := gateFunc(func(context.Context, *http.Request, string) (billingauth.Principal, error) {
		return billingauth.Principal{MerchantID: merchantA, Subject: userB, UserContext: billingauth.UserContext{UserID: userB}}, nil
	})
	table := &router.Table{}
	RegisterMerchantActionRoutes(router.NewMux(table, "/m", nil), nil, Options{Gate: allow, AdminLimiter: middleware.NewAdminOperationLimiter(nil)})
	h := table.Handler()
	preview := "/m/subscriptions/" + userA + "/change-tier/preview"
	// A malformed body answers from the handler without a runtime.
	for range 12 {
		require.NotEqual(t, http.StatusTooManyRequests, doBody(h, http.MethodPost, preview, "{", nil).Code)
	}
	action := "/m/subscriptions/" + userA + "/change-tier"
	for range 10 {
		require.NotEqual(t, http.StatusTooManyRequests, doBody(h, http.MethodPost, action, "{", nil).Code)
	}
	rec := doBody(h, http.MethodPost, action, "{", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))

	service := &router.Table{}
	RegisterServiceRoutes(router.NewMux(service, "/m", nil), nil, Options{Gate: allow})
	rec = httptest.NewRecorder()
	service.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/m/admissions", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, "an authorized call reaches its handler")
}

// Buyer routes: checkout and Solana enrollment authenticate first; Solana
// routes exist only for configured rails, enrollment only with a signer.
func TestUserRoutes(t *testing.T) {
	inventory := func(providers routesurface.ProviderRoutes) []string {
		table := &router.Table{}
		RegisterUserRoutes(router.NewMux(table, "/v1", nil), nil, Options{ProviderRoutes: &providers})
		return routeKeys(table)
	}
	none := inventory(routesurface.ProviderRoutes{})
	require.Subset(t, none, []string{"GET /v1/products", "GET /v1/prices", "GET /v1/checkout-config", "GET /v1/currencies", "POST /v1/checkout", "POST /v1/checkout/{id}/confirm"})
	for _, key := range none {
		require.NotContains(t, key, "solana")
	}
	oneOff := inventory(routesurface.ProviderRoutes{Solana: true})
	require.Subset(t, oneOff, []string{"GET /v1/solana/config", "POST /v1/checkout/{id}/solana-pay"})
	require.NotContains(t, oneOff, "POST /v1/solana/recurring/enroll")
	require.Contains(t, inventory(routesurface.ProviderRoutes{SolanaSigning: true}), "POST /v1/solana/recurring/enroll")

	for _, tc := range []struct {
		name, token, subject string
		status               int
	}{
		{"anonymous", "", "", http.StatusUnauthorized},
		{"invalid token", "bad", "", http.StatusUnauthorized},
		{"opaque subject", "valid", "user-1", http.StatusUnauthorized},
		{"authenticated reaches enrollment", "valid", userA, http.StatusServiceUnavailable},
	} {
		calls := 0
		authn := billingauth.AuthenticatorFunc(func(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
			calls++
			if r.Header.Get("Authorization") != "Bearer valid" {
				return billingauth.UserContext{}, errors.New("invalid token")
			}
			return billingauth.UserContext{UserID: tc.subject}, nil
		})
		rt := &app.Runtime{}
		providers := routesurface.ProviderRoutes{SolanaSigning: true}
		mux := http.NewServeMux()
		RegisterUserRoutes(router.NewMux(mux, "/v1", rt), rt, Options{Authenticator: authn, ProviderRoutes: &providers})
		header := map[string]string{}
		if tc.token != "" {
			header["Authorization"] = "Bearer " + tc.token
		}
		rec := do(mux, http.MethodPost, "/v1/solana/recurring/enroll", header)
		require.Equal(t, tc.status, rec.Code, tc.name)
		require.Equal(t, 1, calls, tc.name)
		require.Equal(t, http.StatusUnauthorized, do(mux, http.MethodPost, "/v1/checkout", nil).Code, "checkout requires authentication")
	}
}

// Platform routes accept only human sessions holding a root-group grant.
func TestPlatformRoutes(t *testing.T) {
	type unlock struct{ user, actor string }
	var unlocked *unlock
	var asked []string
	root := rootFunc(func(_ context.Context, userID, perm string) (bool, error) {
		asked = append(asked, perm)
		return userID == userA, nil
	})
	unlocker := unlockFunc(func(_ context.Context, user, actor string) error {
		unlocked = &unlock{user, actor}
		return nil
	})
	mount := func(opts PlatformOptions) http.Handler {
		mux := http.NewServeMux()
		RegisterPlatformRoutes(router.NewMux(mux, "/v1/platform", nil), nil, opts)
		return mux
	}
	h := mount(PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), Root: root, AdminLimiter: unlocker})
	rec := do(h, http.MethodDelete, "/v1/platform/admin-rate-limit-lockouts/"+userB, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, []string{permissions.RootAdminRateLimitsUnlock}, asked)
	require.Equal(t, &unlock{userB, userA}, unlocked)

	unlocked = nil
	require.Equal(t, http.StatusBadRequest, do(h, http.MethodDelete, "/v1/platform/admin-rate-limit-lockouts/not-a-uuid", nil).Code)
	require.Nil(t, unlocked)

	for _, tc := range []struct {
		name   string
		opts   PlatformOptions
		status int
	}{
		{"no root grant", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userB}, nil), Root: root, AdminLimiter: unlocker}, 403},
		{"unauthenticated", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{}, billingauth.ErrUnauthenticated), Root: root, AdminLimiter: unlocker}, 401},
		{"opaque subject", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: "root"}, nil), Root: root, AdminLimiter: unlocker}, 401},
		{"root checker failure", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), Root: rootFunc(func(context.Context, string, string) (bool, error) { return false, errors.New("db") }), AdminLimiter: unlocker}, 500},
		{"not wired", PlatformOptions{AdminLimiter: unlocker}, 500},
		{"no unlocker", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), Root: root}, 503},
	} {
		require.Equal(t, tc.status, do(mount(tc.opts), http.MethodDelete, "/v1/platform/admin-rate-limit-lockouts/"+userB, nil).Code, tc.name)
		require.Nil(t, unlocked, tc.name)
	}

	asked = nil
	for path, perm := range map[string]string{
		"GET /v1/platform/merchants":            permissions.RootMerchantsRead,
		"GET /v1/platform/merchants/x":          permissions.RootMerchantsRead,
		"DELETE /v1/platform/merchants/x":       permissions.RootMerchantsDelete,
		"POST /v1/platform/merchants/x/restore": permissions.RootMerchantsRestore,
		"GET /v1/platform/worker-health":        permissions.RootWorkerHealthRead,
	} {
		asked = nil
		method, p, _ := strings.Cut(path, " ")
		denied := mount(PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userB}, nil), Root: root})
		require.Equal(t, http.StatusForbidden, do(denied, method, p, nil).Code, path)
		require.Equal(t, []string{perm}, asked, path)
	}
}

type rootFunc func(context.Context, string, string) (bool, error)

func (f rootFunc) HasRootPermission(ctx context.Context, userID, perm string) (bool, error) {
	return f(ctx, userID, perm)
}

type unlockFunc func(context.Context, string, string) error

func (f unlockFunc) Unlock(ctx context.Context, userID, actorID string) error {
	return f(ctx, userID, actorID)
}
