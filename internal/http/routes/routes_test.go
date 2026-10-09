package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	auth "github.com/open-rails/helpers/auth"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/requestauth"
)

// deny admits everyone and refuses every permission but the allowed ones,
// recording each one asked.
type deny struct {
	recordingAuth
	allowed map[string]bool
	asked   []string
}

func (g *deny) RequirePermission(perm string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.asked = append(g.asked, perm)
			if !g.allowed[perm] {
				billingauth.WriteRefusal(w, r, billingauth.Refusal(billing.CodePermissionRequired))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
	RegisterMerchantRoutes(router.NewMux(table, "/v1", rt), rt, opts, Merchant, MerchantConfig)
	return table
}

// Every staff route asks the gate before its handler, for the guard of the
// level its blast radius sets: a read, a write on customers, or the merchant's
// own configuration.
func TestMerchantRouteAuthorization(t *testing.T) {
	gate := &deny{recordingAuth: recordingAuth{who: authtest.User(userA)}}
	rt := gatedRuntime(t)
	table := merchantSurface(rt, Options{Auth: gate, Guard: levelGuard(t)})
	h := table.Handler()
	asked := map[string]string{}
	for _, key := range routeKeys(table) {
		method, path, _ := strings.Cut(key, " ")
		gate.asked = nil
		rec := do(h, method, wildcard.ReplaceAllString(path, "x"), nil)
		require.Equal(t, http.StatusForbidden, rec.Code, key)
		require.Len(t, gate.asked, 1, "%s reached its handler without authorization, or asked twice", key)
		asked[key] = gate.asked[0]
	}

	read, write, admin := staffGuards[StaffReadsKey], staffGuards[StaffWritesKey], staffGuards[MerchantConfigKey]
	for key, perm := range map[string]string{
		"GET /v1/merchant/billing-archive":                                                  admin,
		"POST /v1/merchant/billing-archive":                                                 admin,
		"POST /v1/merchant/billing-import":                                                  admin,
		"GET /v1/merchant/host-events":                                                      read,
		"POST /v1/merchant/host-events/acknowledge":                                         write,
		"GET /v1/merchant/customers/{customer_id}/entitlements":                             read,
		"POST /v1/merchant/customers/ensure":                                                write,
		"POST /v1/merchant/customers/lookup":                                                read,
		"GET /v1/merchant/customers/{customer_id}/delinquency":                              read,
		"GET /v1/merchant/delinquency":                                                      read,
		"GET /v1/merchant/customers/{customer_id}/payment-settlement-status":                read,
		"PUT /v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}":    write,
		"DELETE /v1/merchant/customers/{customer_id}/spend-delegations/{scope}/{scope_key}": write,
		"DELETE /v1/merchant/customers/{customer_id}/payment-methods/{id}":                  write,
		"POST /v1/merchant/customers/{customer_id}/payments/off-channel":                    write,
		"POST /v1/merchant/product-access":                                                  write,
		"PUT /v1/merchant/customers/{customer_id}/rate-overrides/{meter_key}":               admin,
		"PUT /v1/merchant/customers/{customer_id}/invoice-profile":                          write,
		"POST /v1/merchant/customers/{customer_id}/credit-grants":                           write,
		"POST /v1/merchant/customers/{customer_id}/credit-grants/{id}/revoke":               write,
		"GET /v1/merchant/customers/{customer_id}/credit-grants":                            read,
		"PUT /v1/merchant/customers/{customer_id}/credit-limit":                             write,
		"PUT /v1/merchant/customers/{customer_id}/trust-level":                              write,
		"POST /v1/merchant/checkout-sessions":                                               write,
		"POST /v1/merchant/admissions":                                                      write,
		"POST /v1/merchant/admissions/{request_id}/capture":                                 write,
		"GET /v1/merchant/admissions/{request_id}":                                          read,
		"POST /v1/merchant/provider-operations":                                             write,
		"GET /v1/merchant/provider-operations/{operation_id}":                               read,
		"POST /v1/merchant/usage-events":                                                    write,
		"GET /v1/merchant/customers/{customer_id}/usage":                                    read,
		"GET /v1/merchant/payments":                                                         read,
		"POST /v1/merchant/payments/{id}/refunds":                                           write,
		"GET /v1/merchant/subscriptions":                                                    read,
		"POST /v1/merchant/subscriptions/{id}/cancel":                                       write,
		"POST /v1/merchant/subscriptions/{id}/change-tier":                                  write,
		"POST /v1/merchant/reprice-batches":                                                 write,
		"GET /v1/merchant/invoices":                                                         read,
		"POST /v1/merchant/invoices/{id}/void":                                              write,
		"POST /v1/merchant/invoices/{id}/payments":                                          write,
		"POST /v1/merchant/invoices/{id}/retry-collection":                                  write,
		"POST /v1/merchant/metrics/query":                                                   read,
		"PUT /v1/merchant/dashboard":                                                        admin,
		"GET /v1/merchant/notifications":                                                    read,
		"POST /v1/merchant/notifications/read":                                              read,
		"GET /v1/merchant/worker-health":                                                    read,
		"GET /v1/merchant/findings/{id}":                                                    read,
		"POST /v1/merchant/findings/{id}/resolve":                                           write,
		"GET /v1/merchant/configuration":                                                    admin,
		"POST /v1/merchant/configuration/applications":                                      admin,
		"PUT /v1/merchant/alert-webhooks/{id}/url":                                          admin,
		"GET /v1/merchant/psps":                                                             admin,
		"POST /v1/merchant/psps":                                                            admin,
		"GET /v1/merchant/psps/{id}":                                                        admin,
		"PATCH /v1/merchant/psps/{id}":                                                      admin,
		"POST /v1/merchant/psps/{id}/archive":                                               admin,
		"POST /v1/merchant/psps/routing-preview":                                            admin,
		"POST /v1/merchant/psps/refresh":                                                    admin,
		"GET /v1/merchant/rails":                                                            admin,
		"GET /v1/merchant/catalog/products":                                                 read,
		"POST /v1/merchant/catalog/offers/lookup":                                           read,
		"POST /v1/merchant/catalog/applications":                                            admin,
		"PUT /v1/merchant/catalog/meters/{key}":                                             admin,
		"DELETE /v1/merchant/catalog/meters/{key}/rate-card":                                admin,
		"POST /v1/merchant/catalog/product-archives":                                        admin,
	} {
		require.Contains(t, asked, key)
		require.Equal(t, perm, asked[key], key)
	}

	// A guard on the refund resource, or on the one route, overrides the
	// level: the product archive refunds, so it is the refunds' too.
	guard, err := ResolveGuards(PlanStaffRoutes(rt, Options{}, Merchant, MerchantConfig), map[GuardKey]string{
		StaffReadsKey: read, StaffWritesKey: write, MerchantConfigKey: admin,
		ResourceKey(ResRefunds): "refunds", RouteKey("CancelSubscription"): "cancel",
	})
	require.NoError(t, err)
	strict := &deny{recordingAuth: recordingAuth{who: authtest.User(userA)}}
	h = merchantSurface(rt, Options{Auth: strict, Guard: guard}).Handler()
	for path, want := range map[string]string{
		"/v1/merchant/payments/x/refunds":       "refunds",
		"/v1/merchant/subscriptions/x/cancel":   "cancel",
		"/v1/merchant/subscriptions/x/resume":   write,
		"/v1/merchant/catalog/product-archives": "refunds",
	} {
		strict.asked = nil
		require.Equal(t, http.StatusForbidden, do(h, http.MethodPost, path, nil).Code, path)
		require.Equal(t, []string{want}, strict.asked, path)
	}

	// Retired and control-plane-only management routes are not mounted.
	for _, key := range routeKeys(table) {
		for _, retired := range []string{"/api-keys", "/team", "/orphans", "/reconcile", "/merchant-configuration", "/checkout-attempts"} {
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
			RegisterMerchantRoutes(router.NewMux(table, "", rt), rt, Options{Auth: authtest.Deny{}, Guard: levelGuard(t)}, MerchantConfig)
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

// The catalog's writes are MerchantConfig's; its reads and lookups, and
// customers' credit grants, are Merchant's.
func TestCatalogWritePolicy(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{}}
	staff, configuration := &router.Table{}, &router.Table{}
	RegisterMerchantRoutes(router.NewMux(staff, "", rt), rt, Options{Auth: authtest.Deny{}, Guard: levelGuard(t)}, Merchant)
	RegisterMerchantRoutes(router.NewMux(configuration, "", rt), rt, Options{Auth: authtest.Deny{}, Guard: levelGuard(t)}, MerchantConfig)
	for _, key := range []string{"GET /merchant/catalog/revision", "GET /merchant/catalog/meters", "GET /merchant/catalog/product-archives/{id}", "GET /merchant/catalog/products", "POST /merchant/catalog/offers/lookup", "GET /merchant/customers/{customer_id}/rate-overrides", "POST /merchant/customers/{customer_id}/credit-grants"} {
		require.Contains(t, routeKeys(staff), key)
	}
	for _, key := range routeKeys(staff) {
		_, path, _ := strings.Cut(key, " ")
		route, ok := Lookup(strings.Fields(key)[0], "/v1"+path)
		require.True(t, ok, key)
		require.False(t, route.CatalogWrite, "%s: a catalog write is MerchantConfig's", key)
	}
	for _, key := range []string{"POST /merchant/catalog/applications", "PUT /merchant/catalog/products/by-key/{product_key}", "PATCH /merchant/catalog/prices/{id}", "DELETE /merchant/catalog/meters/{key}/rate-card", "POST /merchant/catalog/product-archives", "POST /merchant/catalog/prices", "PUT /merchant/customers/{customer_id}/rate-overrides/{meter_key}", "DELETE /merchant/customers/{customer_id}/rate-overrides/{meter_key}"} {
		require.Contains(t, routeKeys(configuration), key)
	}

	called := false
	closed := &app.Runtime{Config: &config.Config{}, CatalogEdits: &catalogpolicy.Exposure{}}
	rec := httptest.NewRecorder()
	catalogWriteGuardMW(closed)(func(*httprequest.Request) { called = true })(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/products", nil), nil))
	require.Equal(t, http.StatusForbidden, rec.Code, "the guard also refuses if a write is ever mounted")
	require.Contains(t, rec.Body.String(), "catalog_updates_disabled")
	require.False(t, called)

	// The embedded Client's own handler registers mutations without a mount
	// publishing them, and the guard admits only its host principal: the
	// process owner.
	inProcess := &router.Table{}
	RegisterMerchantRoutes(router.NewMux(inProcess, "", closed), closed, HostOptions(), Merchant, MerchantConfig)
	require.Contains(t, routeKeys(inProcess), "POST /merchant/catalog/applications")
	owner := httptest.NewRequest(http.MethodPost, "/products", nil)
	owner = owner.WithContext(requestauth.WithHostPrincipal(owner.Context(), &requestauth.HostPrincipal{}))
	rec = httptest.NewRecorder()
	catalogWriteGuardMW(closed)(func(*httprequest.Request) { called = true })(httprequest.NewHTTP(rec, owner, nil))
	require.True(t, called)

	// Once a mount publishes catalog edits, the guard admits other callers.
	require.NoError(t, closed.CatalogEdits.Decide(true))
	called = false
	rec = httptest.NewRecorder()
	catalogWriteGuardMW(closed)(func(*httprequest.Request) { called = true })(httprequest.NewHTTP(rec, httptest.NewRequest(http.MethodPost, "/products", nil), nil))
	require.True(t, called)
}

// Administrative velocity limits apply per operation class after
// authorization; previews never consume the mutation allowance.
func TestAdminOperationLimits(t *testing.T) {
	rt := gatedRuntime(t)
	table := &router.Table{}
	RegisterMerchantRoutes(router.NewMux(table, "/m", rt), rt, Options{Auth: &recordingAuth{who: authtest.User(userB)}, AdminLimiter: middleware.NewAdminOperationLimiter(nil), Guard: levelGuard(t)}, Merchant)
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
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
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
