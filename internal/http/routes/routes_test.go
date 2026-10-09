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
	RegisterStaffRoutes(router.NewMux(table, "/v1", rt), rt, opts)
	return table
}

// Every staff route asks the gate before its handler, for the permission of
// the bundle its blast radius sets: a read, a write on customers, or the
// merchant's own configuration.
func TestMerchantRouteAuthorization(t *testing.T) {
	gate := &deny{recordingAuth: recordingAuth{who: authtest.User(userA)}}
	rt := gatedRuntime(t)
	table := merchantSurface(rt, Options{Auth: gate, Permissions: staffPermissions})
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

	read, write, catalog, admin := staffPermissions.AdminRead, staffPermissions.AdminWrite, staffPermissions.CatalogWrite, staffPermissions.MerchantConfig
	for key, perm := range map[string]string{
		"GET /v1/admin/billing-archive":                                                  admin,
		"POST /v1/admin/billing-archive":                                                 admin,
		"POST /v1/admin/billing-import":                                                  admin,
		"GET /v1/admin/host-events":                                                      read,
		"POST /v1/admin/host-events/acknowledge":                                         write,
		"GET /v1/admin/customers/{customer_id}/entitlements":                             read,
		"POST /v1/admin/tiers/lookup":                                                    read,
		"GET /v1/admin/customers/{customer_id}/delinquency":                              read,
		"GET /v1/admin/delinquency":                                                      read,
		"GET /v1/admin/customers/{customer_id}/payment-settlement-status":                read,
		"PUT /v1/admin/customers/{customer_id}/spend-delegations":                        write,
		"DELETE /v1/admin/customers/{customer_id}/spend-delegations/{scope}/{scope_key}": write,
		"DELETE /v1/admin/customers/{customer_id}/payment-methods/{id}":                  write,
		"POST /v1/admin/customers/{customer_id}/payments/off-channel":                    write,
		"POST /v1/admin/product-access":                                                  write,
		"PUT /v1/admin/customers/{customer_id}/rate-overrides/{meter_key}":               catalog,
		"PATCH /v1/admin/customers/settings":                                             write,
		"GET /v1/admin/customers/settings":                                               read,
		"POST /v1/admin/credit-grants":                                                   write,
		"POST /v1/admin/customers/{customer_id}/credit-grants/{id}/revoke":               write,
		"GET /v1/admin/customers/{customer_id}/credit-grants":                            read,
		"POST /v1/admin/checkout-sessions":                                               write,
		"POST /v1/admin/admissions":                                                      write,
		"POST /v1/admin/admissions/{request_id}/capture":                                 write,
		"POST /v1/admin/admissions/release":                                              write,
		"POST /v1/admin/admissions/extend":                                               write,
		"POST /v1/admin/wasted-spend":                                                    write,
		"GET /v1/admin/admissions/{request_id}":                                          read,
		"POST /v1/admin/provider-operations":                                             write,
		"GET /v1/admin/provider-operations/{operation_id}":                               read,
		"GET /v1/admin/provider-operations":                                              read,
		"POST /v1/admin/provider-operations/{operation_id}/refusal":                      write,
		"POST /v1/admin/provider-operations/{operation_id}/close":                        write,
		"POST /v1/admin/usage-events":                                                    write,
		"GET /v1/admin/customers/{customer_id}/usage":                                    read,
		"GET /v1/admin/payments":                                                         read,
		"POST /v1/admin/payments/{id}/refunds":                                           write,
		"GET /v1/admin/subscriptions":                                                    read,
		"POST /v1/admin/subscriptions/{id}/cancel":                                       write,
		"POST /v1/admin/subscriptions/{id}/change-tier":                                  write,
		"POST /v1/admin/subscriptions/{id}/change-tier/preview":                          read,
		"POST /v1/admin/price-migrations/preview":                                        read,
		"POST /v1/admin/customers/{customer_id}/entitlements/check":                      read,
		"POST /v1/admin/customers/{customer_id}/product-access/check":                    read,
		"POST /v1/admin/price-migrations":                                                write,
		"GET /v1/admin/invoices":                                                         read,
		"POST /v1/admin/invoices/{id}/void":                                              write,
		"POST /v1/admin/invoices/{id}/payments":                                          write,
		"POST /v1/admin/invoices/{id}/retry-collection":                                  write,
		"POST /v1/admin/metrics/query":                                                   read,
		"PUT /v1/admin/dashboard":                                                        admin,
		"GET /v1/admin/notifications":                                                    read,
		"POST /v1/admin/notifications/read":                                              read,
		"GET /v1/admin/worker-health":                                                    read,
		"GET /v1/admin/findings/{id}":                                                    read,
		"POST /v1/admin/findings/{id}/resolve":                                           write,
		"GET /v1/admin/configuration":                                                    admin,
		"POST /v1/admin/configuration/applications":                                      admin,
		"PUT /v1/admin/alert-webhooks/{id}/url":                                          admin,
		"GET /v1/admin/psps":                                                             admin,
		"POST /v1/admin/psps":                                                            admin,
		"GET /v1/admin/psps/{id}":                                                        admin,
		"PATCH /v1/admin/psps/{id}":                                                      admin,
		"POST /v1/admin/psps/{id}/archive":                                               admin,
		"POST /v1/admin/psps/routing-preview":                                            admin,
		"POST /v1/admin/psps/refresh":                                                    admin,
		"GET /v1/admin/rails":                                                            admin,
		"GET /v1/admin/catalog/products":                                                 read,
		"POST /v1/admin/catalog/offers/lookup":                                           read,
		"POST /v1/admin/catalog/applications":                                            catalog,
		"PUT /v1/admin/catalog/meters/{key}":                                             catalog,
		"DELETE /v1/admin/catalog/meters/{key}/rate-card":                                catalog,
		"POST /v1/admin/catalog/product-archives":                                        catalog,
	} {
		require.Contains(t, asked, key)
		require.Equal(t, perm, asked[key], key)
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
			RegisterStaffRoutes(router.NewMux(table, "", rt), rt, Options{Auth: authtest.Deny{}, Permissions: Permissions{MerchantConfig: "staff:admin"}})
			keys := routeKeys(table)
			for _, key := range []string{
				"GET /admin/configuration", "POST /admin/configuration/applications",
				"GET /admin/psps", "POST /admin/psps", "PATCH /admin/psps/{id}",
				"POST /admin/psps/{id}/archive", "POST /admin/alert-webhooks", "PUT /admin/alert-webhooks/{id}/url",
			} {
				require.Contains(t, keys, key, "%s/%v", backend, writable)
			}
		}
	}
}

// The catalog's writes are CatalogWrite's; its reads and lookups, and
// customers' credit grants, are Admin's.
func TestCatalogWritePolicy(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{}}
	staff, edits := &router.Table{}, &router.Table{}
	RegisterStaffRoutes(router.NewMux(staff, "", rt), rt, Options{Auth: authtest.Deny{}, Permissions: Permissions{AdminRead: "staff:read", AdminWrite: "staff:write"}})
	RegisterStaffRoutes(router.NewMux(edits, "", rt), rt, Options{Auth: authtest.Deny{}, Permissions: Permissions{AdminRead: "staff:read", CatalogWrite: "staff:catalog"}})
	for _, key := range []string{"GET /admin/catalog/revision", "GET /admin/catalog/meters", "GET /admin/catalog/product-archives/{id}", "GET /admin/catalog/products", "POST /admin/catalog/offers/lookup", "GET /admin/customers/{customer_id}/rate-overrides", "POST /admin/credit-grants"} {
		require.Contains(t, routeKeys(staff), key)
	}
	for _, key := range routeKeys(staff) {
		_, path, _ := strings.Cut(key, " ")
		route, ok := Lookup(strings.Fields(key)[0], "/v1"+path)
		require.True(t, ok, key)
		require.NotEqual(t, CatalogWrite, route.Group, "%s: a catalog write is CatalogWrite's", key)
	}
	for _, key := range []string{"POST /admin/catalog/applications", "PUT /admin/catalog/products/by-key/{product_key}", "PATCH /admin/catalog/prices/{id}", "DELETE /admin/catalog/meters/{key}/rate-card", "POST /admin/catalog/product-archives", "POST /admin/catalog/prices", "PUT /admin/customers/{customer_id}/rate-overrides/{meter_key}", "DELETE /admin/customers/{customer_id}/rate-overrides/{meter_key}"} {
		require.Contains(t, routeKeys(edits), key)
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
	RegisterStaffRoutes(router.NewMux(inProcess, "", closed), closed, HostOptions())
	require.Contains(t, routeKeys(inProcess), "POST /admin/catalog/applications")
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
	RegisterStaffRoutes(router.NewMux(table, "/m", rt), rt, Options{Auth: &recordingAuth{who: authtest.User(userB)}, AdminLimiter: middleware.NewAdminOperationLimiter(nil), Permissions: Permissions{AdminRead: "staff:read", AdminWrite: "staff:write"}})
	h := table.Handler()
	preview := "/m/admin/subscriptions/" + userA + "/change-tier/preview"
	// A malformed body answers from the handler without a runtime.
	for range 12 {
		require.NotEqual(t, http.StatusTooManyRequests, doBody(h, http.MethodPost, preview, "{", nil).Code)
	}
	action := "/m/admin/subscriptions/" + userA + "/change-tier"
	for range 10 {
		require.NotEqual(t, http.StatusTooManyRequests, doBody(h, http.MethodPost, action, "{", nil).Code)
	}
	rec := doBody(h, http.MethodPost, action, "{", nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	require.NotEmpty(t, rec.Header().Get("Retry-After"))

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/m/admin/admissions", nil))
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
	require.Subset(t, none, []string{"GET /v1/catalog/products",
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
