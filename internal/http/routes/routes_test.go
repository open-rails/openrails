package routes

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

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
		"GET /v1/admin/entitlements":                                                     read,
		"POST /v1/admin/tiers/lookup":                                                    read,
		"GET /v1/admin/customers/{customer_id}":                                          read,
		"GET /v1/admin/orders":                                                           read,
		"PUT /v1/admin/customers/{customer_id}/spend-delegations":                        write,
		"DELETE /v1/admin/customers/{customer_id}/spend-delegations/{scope}/{scope_key}": write,
		"DELETE /v1/admin/customers/{customer_id}/payment-methods/{id}":                  write,
		"POST /v1/admin/payments":                                                        write,
		"POST /v1/admin/product-access":                                                  write,
		"PUT /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}":                 catalog,
		"PATCH /v1/admin/customers/{customer_id}":                                        write,
		"POST /v1/admin/credit-grants":                                                   write,
		"POST /v1/admin/customers/{customer_id}/credit-grants/{id}/revoke":               write,
		"GET /v1/admin/customers/{customer_id}/credit-grants":                            read,
		"POST /v1/admin/checkout-sessions":                                               write,
		"POST /v1/admin/admissions":                                                      write,
		"POST /v1/admin/admissions/{request_id}/capture":                                 write,
		"POST /v1/admin/admissions/release":                                              write,
		"POST /v1/admin/admissions/extend":                                               write,
		"GET /v1/admin/admissions/{request_id}":                                          read,
		"POST /v1/admin/provider-operations":                                             write,
		"GET /v1/admin/provider-operations/{operation_id}":                               read,
		"GET /v1/admin/provider-operations":                                              read,
		"POST /v1/admin/provider-operations/{operation_id}/increment":                    write,
		"POST /v1/admin/provider-operations/{operation_id}/observations":                 write,
		"POST /v1/admin/provider-operations/{operation_id}/close":                        write,
		"POST /v1/admin/usage-events":                                                    write,
		"GET /v1/admin/payments":                                                         read,
		"POST /v1/admin/payments/{id}/refunds":                                           write,
		"GET /v1/admin/subscriptions":                                                    read,
		"POST /v1/admin/subscriptions/{id}/cancel":                                       write,
		"POST /v1/admin/subscriptions/{id}/change":                                       write,
		"POST /v1/admin/subscriptions/{id}/change/preview":                               read,
		"POST /v1/admin/price-migrations/preview":                                        read,
		"GET /v1/admin/product-access":                                                   read,
		"POST /v1/admin/price-migrations":                                                write,
		"GET /v1/admin/invoices":                                                         read,
		"POST /v1/admin/invoices/{id}/void":                                              write,
		"POST /v1/admin/invoices/{id}/retry-collection":                                  write,
		"POST /v1/admin/metrics/query":                                                   read,
		"PUT /v1/admin/dashboard":                                                        admin,
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
		"GET /v1/admin/catalog/products":                                                 read,
		"GET /v1/admin/catalog/prices/{id}/history":                                      read,
		"GET /v1/admin/catalog/rate-overrides":                                           read,
		"POST /v1/admin/catalog/applications":                                            catalog,
		"PUT /v1/admin/catalog/meters/{key}":                                             catalog,
		"DELETE /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}":              catalog,
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
	for _, key := range []string{"GET /admin/catalog/revision", "GET /admin/catalog/meters", "GET /admin/catalog/product-archives/{id}", "GET /admin/catalog/products", "GET /admin/catalog/prices/{id}/history", "GET /admin/catalog/rate-overrides", "POST /admin/credit-grants"} {
		require.Contains(t, routeKeys(staff), key)
	}
	for _, key := range routeKeys(staff) {
		_, path, _ := strings.Cut(key, " ")
		route, ok := Lookup(strings.Fields(key)[0], "/v1"+path)
		require.True(t, ok, key)
		require.NotEqual(t, CatalogWrite, route.Group, "%s: a catalog write is CatalogWrite's", key)
	}
	for _, key := range []string{"POST /admin/catalog/applications", "PUT /admin/catalog/products/by-key/{product_key}", "PATCH /admin/catalog/prices/{id}", "PUT /admin/catalog/meters/{key}", "POST /admin/catalog/product-archives", "POST /admin/catalog/prices", "PUT /admin/catalog/rate-overrides/{customer_id}/{meter_key}", "DELETE /admin/catalog/rate-overrides/{customer_id}/{meter_key}"} {
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
	preview := "/m/admin/subscriptions/" + userA + "/change/preview"
	// A malformed body answers from the handler without a runtime.
	for range 12 {
		require.NotEqual(t, http.StatusTooManyRequests, doBody(h, http.MethodPost, preview, "{", nil).Code)
	}
	action := "/m/admin/subscriptions/" + userA + "/change"
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
