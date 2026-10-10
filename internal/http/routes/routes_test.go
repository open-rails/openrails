package routes

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/middleware"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

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
// the group its blast radius sets: a read, a write on customers, or the
// merchant's own configuration.
func TestMerchantRouteAuthorization(t *testing.T) {
	gate := &recordingAuth{who: authtest.User(userA), allowed: map[string]bool{}}
	rt := gatedRuntime(t)
	table := merchantSurface(rt, Options{Auth: gate, Scope: testScope, Permissions: staffPermissions})
	h := table.Handler()
	asked := map[string]string{}
	for _, key := range routeKeys(table) {
		if key == "GET /v1/admin/access" {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		rec := do(h, method, wildcard.ReplaceAllString(path, "x"), nil)
		require.Equal(t, http.StatusForbidden, rec.Code, key)
		var perms []string
		for _, call := range gate.take() {
			if perm, ok := strings.CutPrefix(call, "Can:"); ok {
				perms = append(perms, perm)
			}
		}
		require.Len(t, perms, 1, "%s reached its handler without authorization, or asked twice", key)
		asked[key] = perms[0]
	}

	read, write, catalog, admin, metrics := staffPermissions.AdminRead, staffPermissions.AdminUpdate, staffPermissions.Catalog, staffPermissions.MerchantConfig, staffPermissions.Metrics
	for key, perm := range map[string]string{
		"GET /v1/admin/billing-archive":                                     admin,
		"POST /v1/admin/billing-archive":                                    admin,
		"POST /v1/admin/billing-import":                                     admin,
		"GET /v1/admin/entitlements":                                        read,
		"POST /v1/admin/product-access":                                     write,
		"POST /v1/admin/product-access/{id}/revoke":                         write,
		"GET /v1/admin/product-access":                                      read,
		"GET /v1/admin/catalog/rate-overrides":                              catalog,
		"PUT /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}":    catalog,
		"GET /v1/admin/customers/{customer_id}":                             read,
		"PATCH /v1/admin/customers/{customer_id}":                           write,
		"POST /v1/admin/credit-grants":                                      write,
		"POST /v1/admin/credit-grants/{id}/revoke":                          write,
		"GET /v1/admin/credit-grants":                                       read,
		"GET /v1/admin/credit-grants/{id}":                                  read,
		"POST /v1/admin/checkout-sessions":                                  write,
		"GET /v1/admin/provider-operations/{operation_id}":                  read,
		"GET /v1/admin/provider-operations":                                 read,
		"POST /v1/admin/provider-operations/{operation_id}/close":           write,
		"GET /v1/admin/payments":                                            read,
		"POST /v1/admin/payments/{id}/refunds":                              write,
		"GET /v1/admin/renewals":                                            read,
		"GET /v1/admin/subscriptions":                                       read,
		"POST /v1/admin/subscriptions/{id}/cancel":                          write,
		"POST /v1/admin/subscriptions/{id}/change":                          write,
		"POST /v1/admin/subscriptions/{id}/change/preview":                  read,
		"POST /v1/admin/price-migrations/preview":                           catalog,
		"POST /v1/admin/price-migrations":                                   catalog,
		"GET /v1/admin/invoices":                                            read,
		"POST /v1/admin/invoices/{id}/void":                                 write,
		"POST /v1/admin/invoices/{id}/mark-uncollectible":                   write,
		"POST /v1/admin/invoices/{id}/retry-collection":                     write,
		"POST /v1/admin/metrics/query":                                      metrics,
		"PUT /v1/admin/dashboard":                                           admin,
		"GET /v1/admin/findings/{id}":                                       read,
		"POST /v1/admin/findings/{id}/resolve":                              write,
		"GET /v1/admin/configuration":                                       admin,
		"PATCH /v1/admin/configuration":                                     admin,
		"PATCH /v1/admin/alert-webhooks/{id}":                               admin,
		"GET /v1/admin/psps":                                                admin,
		"POST /v1/admin/psps":                                               admin,
		"GET /v1/admin/psps/{id}":                                           admin,
		"PATCH /v1/admin/psps/{id}":                                         admin,
		"POST /v1/admin/psps/routing-preview":                               admin,
		"POST /v1/admin/psps/refresh":                                       write,
		"GET /v1/admin/catalog/products":                                    catalog,
		"GET /v1/admin/catalog/prices/{id}/history":                         catalog,
		"POST /v1/admin/catalog/applications":                               catalog,
		"PUT /v1/admin/catalog/meters/{key}":                                catalog,
		"DELETE /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}": catalog,
		"POST /v1/admin/catalog/product-archives":                           catalog,
		"GET /v1/admin/orders":                                              read,
		"POST /v1/admin/payments":                                           write,
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

// Merchant configuration reads mount whatever holds it; its edits mount only
// where Vault does: a file is read-only.
func TestConfigurationEditsMountOnlyWhereEditable(t *testing.T) {
	reads := []string{"GET /admin/configuration", "GET /admin/psps", "GET /admin/psps/{id}", "GET /admin/alert-webhooks"}
	edits := []string{"PATCH /admin/configuration", "POST /admin/psps", "PATCH /admin/psps/{id}", "POST /admin/alert-webhooks", "PATCH /admin/alert-webhooks/{id}", "DELETE /admin/alert-webhooks/{id}"}
	for _, editable := range []bool{false, true} {
		cfg := &config.Config{}
		if editable {
			cfg.Vault = &config.VaultConfig{KVMount: "kv"}
		}
		rt := &app.Runtime{Config: cfg}
		table := &router.Table{}
		RegisterStaffRoutes(router.NewMux(table, "", rt), rt, Options{Auth: authtest.Deny{}, Scope: testScope, Permissions: Permissions{MerchantConfig: "staff:admin"}})
		keys := routeKeys(table)
		for _, key := range reads {
			require.Contains(t, keys, key, "editable=%v", editable)
		}
		for _, key := range edits {
			if editable {
				require.Contains(t, keys, key)
			} else {
				require.NotContains(t, keys, key)
			}
		}
	}
}

// The catalog, its reads and edits and price migrations, is Catalog's;
// customers' credit grants and the PSP refresh are customer support's.
func TestCatalogWritePolicy(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{}}
	staff, edits := &router.Table{}, &router.Table{}
	RegisterStaffRoutes(router.NewMux(staff, "", rt), rt, Options{Auth: authtest.Deny{}, Scope: testScope, Permissions: Permissions{AdminRead: "staff:read", AdminUpdate: "staff:write"}})
	RegisterStaffRoutes(router.NewMux(edits, "", rt), rt, Options{Auth: authtest.Deny{}, Scope: testScope, Permissions: Permissions{Catalog: "staff:catalog"}})
	for _, key := range []string{"POST /admin/credit-grants", "POST /admin/psps/refresh"} {
		require.Contains(t, routeKeys(staff), key)
	}
	for _, key := range routeKeys(staff) {
		_, path, _ := strings.Cut(key, " ")
		route, ok := Lookup(strings.Fields(key)[0], "/v1"+path)
		require.True(t, ok, key)
		require.NotEqual(t, CatalogAdmin, route.Group, "%s: the catalog is Catalog's", key)
	}
	for _, key := range []string{"GET /admin/catalog/revision", "GET /admin/catalog/meters", "GET /admin/catalog/products", "GET /admin/catalog/rate-overrides", "GET /admin/price-migrations", "POST /admin/price-migrations/preview"} {
		require.Contains(t, routeKeys(edits), key)
	}
	for _, key := range []string{"POST /admin/catalog/applications", "PATCH /admin/catalog/prices/{id}", "POST /admin/catalog/product-archives", "POST /admin/catalog/prices", "PUT /admin/catalog/rate-overrides/{customer_id}/{meter_key}", "DELETE /admin/catalog/rate-overrides/{customer_id}/{meter_key}"} {
		require.Contains(t, routeKeys(edits), key)
	}

	// The embedded Client's own handler registers every mutation: the process
	// owner edits its catalog whatever a mount publishes.
	inProcess := &router.Table{}
	RegisterStaffRoutes(router.NewMux(inProcess, "", rt), rt, HostOptions())
	require.Contains(t, routeKeys(inProcess), "POST /admin/catalog/applications")
}

// Administrative velocity limits apply per operation class after
// authorization; previews never consume the mutation allowance.
func TestAdminOperationLimits(t *testing.T) {
	rt := gatedRuntime(t)
	table := &router.Table{}
	RegisterStaffRoutes(router.NewMux(table, "/m", rt), rt, Options{Auth: &recordingAuth{who: authtest.User(userB)}, AdminLimiter: middleware.NewAdminOperationLimiter(abusestate.New(nil)), Scope: testScope, Permissions: Permissions{AdminRead: "staff:read", AdminUpdate: "staff:write"}})
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
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/m/admin/subscriptions/"+userA+"/resume", nil))
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
