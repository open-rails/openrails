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

	auth "github.com/open-rails/helpers/auth"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/requestauth"
)

// deny records every permission asked of it and refuses.
type deny struct{ asked []string }

func (g *deny) Authorize(_ context.Context, _ *http.Request, perm string) (billingauth.Principal, error) {
	g.asked = append(g.asked, perm)
	return billingauth.Principal{}, billingauth.Refusal(billing.CodePermissionRequired)
}

func (*deny) RequireRecentSignIn(context.Context, *http.Request, billingauth.Principal) error {
	return nil
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
	RegisterMerchantRoutes(router.NewMux(table, "/v1", rt), rt, opts)
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

	p := billing.MerchantCustomerSettingsRead
	w := billing.MerchantCustomerSettingsUpdate
	for key, perm := range map[string]string{
		"GET /v1/merchant/billing-archive":                                                  billing.MerchantBillingExport,
		"POST /v1/merchant/billing-archive":                                                 billing.MerchantBillingImport,
		"POST /v1/merchant/billing-import":                                                  billing.MerchantBillingImport,
		"GET /v1/merchant/host-events":                                                      billing.MerchantHostEventsRead,
		"POST /v1/merchant/host-events/{id}/acknowledge":                                    billing.MerchantHostEventsAcknowledge,
		"POST /v1/merchant/entitlements/lookup":                                             p,
		"PUT /v1/merchant/customers/{customer_id}":                                          w,
		"GET /v1/merchant/customers/{customer_id}":                                          p,
		"GET /v1/merchant/customers/{customer_id}/delinquency":                              p,
		"GET /v1/merchant/delinquency":                                                      p,
		"GET /v1/merchant/customers/{customer_id}/payment-settlement-status":                billing.MerchantPaymentsRead,
		"PUT /v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}":    w,
		"DELETE /v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}": w,
		"DELETE /v1/merchant/customers/{customer_id}/payment-methods/{id}":                  w,
		"POST /v1/merchant/customers/{customer_id}/payments/off-channel":                    w,
		"POST /v1/merchant/customers/{customer_id}/entitlements":                            w,
		"PUT /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}":               w,
		"PUT /v1/merchant/customers/{customer_id}/invoice-profile":                          w,
		"POST /v1/merchant/customers/{customer_id}/credit-grants":                           billing.MerchantCreditsGrant,
		"POST /v1/merchant/customers/{customer_id}/credit-grants/{id}/revoke":               billing.MerchantCreditsRevoke,
		"GET /v1/merchant/customers/{customer_id}/credit-grants":                            p,
		"PUT /v1/merchant/customers/{customer_id}/credit-limit":                             billing.MerchantCreditsGrant,
		"PUT /v1/merchant/customers/{customer_id}/trust-level":                              w,
		"POST /v1/merchant/checkout-sessions":                                               billing.MerchantCheckoutCreate,
		"POST /v1/merchant/checkout-attempts":                                               billing.MerchantCheckoutCreate,
		"GET /v1/merchant/checkout-attempts/{id}":                                           p,
		"POST /v1/merchant/admissions":                                                      billing.MerchantAdmissionsCreate,
		"POST /v1/merchant/admissions/{request_id}/capture":                                 billing.MerchantAdmissionsCreate,
		"GET /v1/merchant/admissions/{request_id}":                                          billing.MerchantUsageRead,
		"POST /v1/merchant/provider-operations":                                             billing.MerchantAdmissionsCreate,
		"GET /v1/merchant/provider-operations/{operation_id}":                               billing.MerchantUsageRead,
		"POST /v1/merchant/usage-events":                                                    billing.MerchantAdmissionsCreate,
		"GET /v1/merchant/customers/{customer_id}/usage":                                    billing.MerchantUsageRead,
		"GET /v1/merchant/payments":                                                         billing.MerchantPaymentsRead,
		"POST /v1/merchant/payments/{id}/refunds":                                           billing.MerchantPaymentsRefund,
		"GET /v1/merchant/subscriptions":                                                    billing.MerchantSubscriptionsRead,
		"POST /v1/merchant/subscriptions/{id}/cancel":                                       billing.MerchantSubscriptionsUpdate,
		"POST /v1/merchant/subscriptions/{id}/change-tier":                                  billing.MerchantSubscriptionsUpdate,
		"POST /v1/merchant/reprice-batches":                                                 billing.MerchantSubscriptionsUpdate,
		"GET /v1/merchant/invoices":                                                         billing.MerchantInvoicesRead,
		"POST /v1/merchant/invoices/{id}/void":                                              billing.MerchantInvoicesUpdate,
		"POST /v1/merchant/invoices/{id}/payments":                                          billing.MerchantInvoicesUpdate,
		"POST /v1/merchant/invoices/{id}/retry-collection":                                  billing.MerchantInvoicesCollect,
		"POST /v1/merchant/metrics/query":                                                   billing.MerchantMetricsRead,
		"PUT /v1/merchant/dashboard":                                                        billing.MerchantDashboardUpdate,
		"GET /v1/merchant/notifications":                                                    billing.MerchantMetricsRead,
		"POST /v1/merchant/notifications/{id}/read":                                         billing.MerchantMetricsRead,
		"GET /v1/merchant/repair-alerts":                                                    billing.MerchantRepairAlertsRead,
		"GET /v1/merchant/worker-health":                                                    billing.MerchantRepairAlertsRead,
		"GET /v1/merchant/findings/{id}":                                                    billing.MerchantRepairAlertsRead,
		"POST /v1/merchant/findings/{id}/resolve":                                           billing.MerchantFindingsResolve,
		"GET /v1/merchant/configuration":                                                    billing.MerchantSettingsRead,
		"POST /v1/merchant/configuration/applications":                                      billing.MerchantSettingsUpdate,
		"PUT /v1/merchant/alert-webhooks/{id}/url":                                          billing.MerchantSettingsUpdate,
		"GET /v1/merchant/psps":                                                             billing.MerchantPSPsRead,
		"POST /v1/merchant/psps":                                                            billing.MerchantPSPsUpdate,
		"GET /v1/merchant/psps/{id}":                                                        billing.MerchantPSPsRead,
		"PATCH /v1/merchant/psps/{id}":                                                      billing.MerchantPSPsUpdate,
		"POST /v1/merchant/psps/{id}/archive":                                               billing.MerchantPSPsUpdate,
		"POST /v1/merchant/psps/routing-preview":                                            billing.MerchantPSPsRead,
		"POST /v1/merchant/psps/refresh":                                                    billing.MerchantSubscriptionsUpdate,
		"GET /v1/merchant/rails":                                                            billing.MerchantPSPsRead,
		"GET /v1/merchant/catalog/products":                                                 billing.MerchantCatalogRead,
		"POST /v1/merchant/catalog/offers/lookup":                                           billing.MerchantCatalogRead,
		"POST /v1/merchant/catalog/applications":                                            billing.MerchantCatalogUpdate,
		"PUT /v1/merchant/catalog/meters/{key}":                                             billing.MerchantCatalogUpdate,
		"DELETE /v1/merchant/catalog/meters/{key}/rate-card":                                billing.MerchantCatalogUpdate,
		"POST /v1/merchant/catalog/product-archives":                                        billing.MerchantCatalogUpdate,
		"POST /v1/merchant/catalogs":                                                        billing.MerchantCatalogUpdate,
		"POST /v1/catalog/offers/lookup":                                                    billing.MerchantCatalogOwnRead,
		"POST /v1/catalog/products":                                                         billing.MerchantCatalogOwnUpdate,
	} {
		require.Contains(t, asked, key)
		require.Equal(t, perm, asked[key], key)
	}

	// Money-moving catalog archives need both catalog and refund authority.
	gate.asked = nil
	allowCatalog := gateFunc(func(ctx context.Context, r *http.Request, perm string) (billingauth.Principal, error) {
		if perm == billing.MerchantCatalogUpdate {
			return billingauth.Principal{MerchantID: merchantA}, nil
		}
		return gate.Authorize(ctx, r, perm)
	})
	rec := do(merchantSurface(rt, Options{Gate: allowCatalog}).Handler(), http.MethodPost, "/v1/merchant/catalog/product-archives", nil)
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, []string{billing.MerchantPaymentsRefund}, gate.asked)

	// Retired and control-plane-only management routes are not mounted.
	for _, key := range routeKeys(table) {
		for _, retired := range []string{"/api-keys", "/team", "/orphans", "/reconcile", "/merchant-configuration", "/api-host"} {
			require.NotContains(t, key, retired)
		}
	}
}

// Provider metadata and lifecycle archives stay mounted (and gated) even when
// the secret backend is read-only.
func TestConfigurationRoutesMountedForEveryBackend(t *testing.T) {
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB, config.SecretBackendVault} {
		for _, writable := range []bool{false, true} {
			rt := &app.Runtime{Config: &config.Config{SecretBackend: backend}, RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable}}
			table := &router.Table{}
			RegisterMerchantRoutes(router.NewMux(table, "", rt), rt, Options{})
			keys := routeKeys(table)
			for _, key := range []string{
				"GET /merchant/configuration", "POST /merchant/configuration/applications",
				"GET /merchant/psps", "POST /merchant/psps", "PATCH /merchant/psps/{id}",
				"POST /merchant/psps/{id}/archive", "POST /merchant/alert-webhooks", "PUT /merchant/alert-webhooks/{id}/url",
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
		merchant := &router.Table{}
		RegisterMerchantRoutes(router.NewMux(merchant, "", rt), rt, Options{})
		all := routeKeys(merchant)
		var keys []string
		for _, key := range all {
			_, path, _ := strings.Cut(key, " ")
			if strings.HasPrefix(path, "/merchant/catalog") || strings.HasPrefix(path, "/catalog") {
				keys = append(keys, key)
			}
		}
		for _, key := range append([]string{"GET /merchant/catalog/revision", "GET /merchant/catalog/meters", "GET /merchant/catalog/product-archives/{id}", "GET /catalog/products", "GET /merchant/catalogs"}, reads...) {
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
			for _, key := range []string{"POST /merchant/catalog/applications", "PUT /merchant/catalog/products/by-key/{key}", "PATCH /merchant/catalog/prices/{id}", "DELETE /merchant/catalog/meters/{key}/rate-card", "POST /merchant/catalog/product-archives", "POST /catalog/prices", "POST /merchant/catalogs"} {
				require.Contains(t, keys, key)
			}
		}

		keys = all
		require.Contains(t, keys, "GET /merchant/customers/{customer_id}/rate-overrides")
		require.Contains(t, keys, "POST /merchant/customers/{customer_id}/credit-grants", "credit grants are not catalog authoring")
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

	// The embedded Client's own handler registers mutations with the flag off,
	// and the guard admits only its host principal: the process owner.
	rt := &app.Runtime{Config: &config.Config{}}
	inProcess := &router.Table{}
	RegisterMerchantRoutes(router.NewMux(inProcess, "", rt), rt, Options{InProcess: true})
	require.Contains(t, routeKeys(inProcess), "POST /merchant/catalog/applications")
	owner := httptest.NewRequest(http.MethodPost, "/products", nil)
	owner = owner.WithContext(requestauth.WithHostPrincipal(owner.Context(), &requestauth.HostPrincipal{}))
	rec = httptest.NewRecorder()
	catalogWriteGuardMW(&config.Config{})(func(*httprequest.Request) { called = true })(httprequest.NewHTTP(rec, owner, nil))
	require.True(t, called)
}

// Administrative velocity limits apply per operation class after
// authorization; previews never consume the mutation allowance.
func TestAdminOperationLimits(t *testing.T) {
	allow := gateFunc(func(context.Context, *http.Request, string) (billingauth.Principal, error) {
		return billingauth.Principal{MerchantID: merchantA, Subject: userB, UserContext: billingauth.UserContext{UserID: userB}}, nil
	})
	table := &router.Table{}
	RegisterMerchantRoutes(router.NewMux(table, "/m", nil), nil, Options{Gate: allow, AdminLimiter: middleware.NewAdminOperationLimiter(nil)})
	h := table.Handler()
	preview := "/m/merchant/subscriptions/" + userA + "/change-tier/preview"
	// A malformed body answers from the handler without a runtime.
	for range 12 {
		require.NotEqual(t, http.StatusTooManyRequests, doBody(h, http.MethodPost, preview, "{", nil).Code)
	}
	action := "/m/merchant/subscriptions/" + userA + "/change-tier"
	for range 10 {
		require.NotEqual(t, http.StatusTooManyRequests, doBody(h, http.MethodPost, action, "{", nil).Code)
	}
	rec := doBody(h, http.MethodPost, action, "{", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/m/merchant/admissions", nil))
	require.Equal(t, http.StatusBadRequest, rec.Code, "an authorized call reaches its handler")
}

// Buyer routes are public or addressed by a checkout session or attempt id;
// Solana routes exist only for configured rails. A browser buys only through
// a checkout session.
func TestUserRoutes(t *testing.T) {
	inventory := func(providers routesurface.ProviderRoutes) []string {
		table := &router.Table{}
		RegisterUserRoutes(router.NewMux(table, "/v1", nil), nil, Options{ProviderRoutes: &providers})
		return routeKeys(table)
	}
	none := inventory(routesurface.ProviderRoutes{})
	require.Subset(t, none, []string{"GET /v1/products", "GET /v1/prices", "GET /v1/checkout-config", "GET /v1/currencies",
		"GET /v1/checkout-sessions/{id}", "POST /v1/checkout-sessions/{id}/pay"})
	for _, key := range none {
		require.NotContains(t, key, "solana")
		require.NotRegexp(t, `/v1/checkout(/|$)`, key, "no engine checkout door for browsers")
	}
	oneOff := inventory(routesurface.ProviderRoutes{Solana: true})
	require.Subset(t, oneOff, []string{"GET /v1/solana/tokens", "POST /v1/checkout-attempts/{id}/solana-pay"})
	for _, key := range inventory(routesurface.ProviderRoutes{Solana: true, SolanaSigning: true}) {
		require.NotContains(t, key, "/solana/recurring")
	}
}

// Platform routes accept only human sessions holding a root-group grant.
func TestPlatformRoutes(t *testing.T) {
	type unlock struct{ user, actor string }
	var unlocked *unlock
	var asked []string
	checker := func(granted bool) rootFunc {
		return func(_ context.Context, _ *http.Request, perm string) (bool, error) {
			asked = append(asked, perm)
			return granted, nil
		}
	}
	root, notRoot := checker(true), checker(false)
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
	require.Equal(t, []string{billing.RootAdminRateLimitsUnlock}, asked)
	require.Equal(t, &unlock{userB, userA}, unlocked)

	unlocked = nil
	require.Equal(t, http.StatusBadRequest, do(h, http.MethodDelete, "/v1/platform/admin-rate-limit-lockouts/not-a-uuid", nil).Code)
	require.Nil(t, unlocked)

	for _, tc := range []struct {
		name   string
		opts   PlatformOptions
		status int
	}{
		{"no root grant", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userB}, nil), Root: notRoot, AdminLimiter: unlocker}, 403},
		{"revoked session", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), Root: rootFunc(func(context.Context, *http.Request, string) (bool, error) { return false, auth.ErrRevoked }), AdminLimiter: unlocker}, 401},
		{"unauthenticated", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{}, billingauth.ErrUnauthenticated), Root: root, AdminLimiter: unlocker}, 401},
		{"opaque subject", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: "root"}, nil), Root: root, AdminLimiter: unlocker}, 401},
		{"root checker failure", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), Root: rootFunc(func(context.Context, *http.Request, string) (bool, error) { return false, errors.New("db") }), AdminLimiter: unlocker}, 500},
		{"not wired", PlatformOptions{AdminLimiter: unlocker}, 500},
		{"no unlocker", PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userA}, nil), Root: root}, 503},
	} {
		require.Equal(t, tc.status, do(mount(tc.opts), http.MethodDelete, "/v1/platform/admin-rate-limit-lockouts/"+userB, nil).Code, tc.name)
		require.Nil(t, unlocked, tc.name)
	}

	asked = nil
	for path, perm := range map[string]string{
		"GET /v1/platform/merchants":            billing.RootMerchantsRead,
		"GET /v1/platform/merchants/x":          billing.RootMerchantsRead,
		"DELETE /v1/platform/merchants/x":       billing.RootMerchantsDelete,
		"POST /v1/platform/merchants/x/restore": billing.RootMerchantsRestore,
		"GET /v1/platform/worker-health":        billing.RootWorkerHealthRead,
	} {
		asked = nil
		method, p, _ := strings.Cut(path, " ")
		denied := mount(PlatformOptions{Authenticator: userAuth(billingauth.UserContext{UserID: userB}, nil), Root: notRoot})
		require.Equal(t, http.StatusForbidden, do(denied, method, p, nil).Code, path)
		require.Equal(t, []string{perm}, asked, path)
	}
}

type rootFunc func(context.Context, *http.Request, string) (bool, error)

func (f rootFunc) HasRootPermission(ctx context.Context, r *http.Request, perm string) (bool, error) {
	return f(ctx, r, perm)
}

type unlockFunc func(context.Context, string, string) error

func (f unlockFunc) Unlock(ctx context.Context, userID, actorID string) error {
	return f(ctx, userID, actorID)
}
