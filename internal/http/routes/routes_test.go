package routes

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
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
// the group its blast radius sets, in both personas: a read, a write on
// customers, or the merchant's own configuration.
func TestMerchantRouteAuthorization(t *testing.T) {
	gate := &recordingAuth{who: authtest.User(userA), allowed: map[billing.Permission]bool{}}
	rt := gatedRuntime(t)
	table := merchantSurface(rt, Options{Auth: gate, Groups: everyGroup, live: live})
	h := table.Handler()
	asked := map[string]Pair{}
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
		require.Len(t, perms, 2, "%s reached its handler without authorization, or asked a name twice", key)
		asked[key] = Pair{Merchant: billing.Permission(perms[0]), Root: billing.Permission(perms[1])}
	}

	read, write, catalog, admin, metrics := needAdminRead, needAdminUpdate, needCatalog, needMerchantConfig, needMetrics
	for key, perm := range map[string]Pair{
		"GET /v1/merchant/billing-archive":                 admin,
		"POST /v1/merchant/billing-archive":                admin,
		"POST /v1/merchant/billing-import":                 admin,
		"GET /v1/admin/entitlements":                       read,
		"POST /v1/admin/product-access":                    write,
		"POST /v1/admin/product-access/{id}/revoke":        write,
		"GET /v1/admin/product-access":                     read,
		"GET /v1/admin/customers/{customer_id}":            read,
		"PATCH /v1/admin/customers/{customer_id}":          write,
		"POST /v1/admin/credit-grants":                     write,
		"POST /v1/admin/credit-grants/{id}/revoke":         write,
		"GET /v1/admin/credit-grants":                      read,
		"GET /v1/admin/credit-grants/{id}":                 read,
		"GET /v1/admin/payments":                           read,
		"POST /v1/admin/payments/{id}/refunds":             write,
		"GET /v1/admin/renewals":                           read,
		"GET /v1/admin/subscriptions":                      read,
		"POST /v1/admin/subscriptions/{id}/cancel":         write,
		"POST /v1/admin/subscriptions/{id}/change":         write,
		"POST /v1/admin/subscriptions/{id}/change/preview": read,
		"POST /v1/catalog/price-migrations/preview":        catalog,
		"POST /v1/catalog/price-migrations":                catalog,
		"POST /v1/admin/orders/{id}/void":                  write,
		"POST /v1/admin/orders/{id}/mark-uncollectible":    write,
		"POST /v1/admin/orders/{id}/retry-collection":      write,
		"POST /v1/metrics/query":                           metrics,
		"PUT /v1/merchant/dashboard":                       admin,
		"GET /v1/merchant/findings":                        admin,
		"GET /v1/merchant/findings/{id}":                   admin,
		"POST /v1/merchant/findings/{id}/resolve":          admin,
		"GET /v1/merchant/configuration":                   admin,
		"PATCH /v1/merchant/configuration":                 admin,
		"PATCH /v1/merchant/alert-webhooks/{id}":           admin,
		"GET /v1/merchant/psps":                            admin,
		"POST /v1/merchant/psps":                           admin,
		"GET /v1/merchant/psps/{id}":                       admin,
		"PATCH /v1/merchant/psps/{id}":                     admin,
		"POST /v1/merchant/psps/routing-preview":           admin,
		"POST /v1/merchant/psps/refresh":                   admin,
		"GET /v1/catalog/products":                         catalog,
		"GET /v1/catalog/prices/{id}/history":              catalog,
		"POST /v1/catalog/apply":                           catalog,
		"PUT /v1/catalog/meters/{key}":                     catalog,
		"GET /v1/admin/orders":                             read,
		"POST /v1/admin/payments":                          write,
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
	reads := []string{"GET /merchant/configuration", "GET /merchant/psps", "GET /merchant/psps/{id}", "GET /merchant/alert-webhooks"}
	edits := []string{"PATCH /merchant/configuration", "POST /merchant/psps", "PATCH /merchant/psps/{id}", "POST /merchant/alert-webhooks", "PATCH /merchant/alert-webhooks/{id}", "DELETE /merchant/alert-webhooks/{id}"}
	for _, editable := range []bool{false, true} {
		cfg := &config.Config{}
		if editable {
			cfg.Vault = &config.VaultConfig{KVMount: "kv"}
		}
		rt := &app.Runtime{Config: cfg}
		table := &router.Table{}
		RegisterStaffRoutes(router.NewMux(table, "", rt), rt, Options{Auth: authtest.Deny{}, Groups: config.RouteGroups{MerchantConfig: true}})
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
// customers' credit grants are customer support's.
func TestCatalogWritePolicy(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{}}
	staff, edits := &router.Table{}, &router.Table{}
	RegisterStaffRoutes(router.NewMux(staff, "", rt), rt, Options{Auth: authtest.Deny{}, Groups: config.RouteGroups{Admin: true}})
	RegisterStaffRoutes(router.NewMux(edits, "", rt), rt, Options{Auth: authtest.Deny{}, Groups: config.RouteGroups{Catalog: true}})
	require.Contains(t, routeKeys(staff), "POST /admin/credit-grants")
	require.NotContains(t, routeKeys(staff), "POST /merchant/psps/refresh", "the PSP pull is the merchant configuration's")
	for _, key := range routeKeys(staff) {
		_, path, _ := strings.Cut(key, " ")
		route, ok := Lookup(strings.Fields(key)[0], "/v1"+path)
		require.True(t, ok, key)
		require.NotEqual(t, CatalogAdmin, route.Group, "%s: the catalog is Catalog's", key)
	}
	for _, key := range []string{"GET /catalog/meters", "GET /catalog/products", "GET /catalog/price-migrations", "POST /catalog/price-migrations/preview"} {
		require.Contains(t, routeKeys(edits), key)
	}
	for _, key := range []string{"POST /catalog/apply", "PATCH /catalog/prices/{id}", "POST /catalog/prices", "PUT /catalog/meters/{key}"} {
		require.Contains(t, routeKeys(edits), key)
	}

	// The embedded Client's own handler registers every mutation: the process
	// owner edits its catalog whatever a mount publishes.
	inProcess := &router.Table{}
	RegisterStaffRoutes(router.NewMux(inProcess, "", rt), rt, HostOptions())
	require.Contains(t, routeKeys(inProcess), "POST /catalog/apply")
}

// Administrative velocity limits apply per operation class after
// authorization; previews never consume the mutation allowance.
func TestAdminOperationLimits(t *testing.T) {
	rt := gatedRuntime(t)
	table := &router.Table{}
	state := abusestate.New(nil)
	at := time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC) // mid-window: no window ends during the test
	state.SetClock(func() time.Time { return at })
	RegisterStaffRoutes(router.NewMux(table, "/m", rt), rt, Options{Auth: &recordingAuth{who: authtest.User(userB)}, AdminLimiter: middleware.NewAdminOperationLimiter(state), Groups: config.RouteGroups{Admin: true}, live: live})
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
	require.Subset(t, none, []string{"GET /v1/products"})
	for _, key := range none {
		require.NotContains(t, key, "solana")
		require.NotRegexp(t, `/v1/checkout(/|$)`, key, "no engine checkout door for browsers")
	}
	oneOff := inventory(routesurface.ProviderRoutes{Solana: true})
	require.Subset(t, oneOff, []string{"POST /v1/orders/{id}/solana-pay"})
	for _, key := range inventory(routesurface.ProviderRoutes{Solana: true, SolanaSigning: true}) {
		require.NotContains(t, key, "/solana/recurring")
	}
}
