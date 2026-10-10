package engine

import (
	"encoding/json"
	pkgcatalog "github.com/open-rails/openrails/catalog"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/routebundle"
)

// httpRuntime is a database-free runtime bound to one merchant: route
// materialization and every refusal exercised here happen before a handler
// touches storage.
func httpRuntime() *Engine {
	c := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}
	rt := &app.Runtime{Config: c}
	rt.SetConfiguredMerchant(billing.MerchantID(uuid.MustParse("11111111-1111-4111-8111-111111111111")))
	return &Engine{App: &app.App{Config: c, Runtime: rt}}
}

// hookAuth admits whom admit says, as the user customer.
type hookAuth struct{ admit func(*http.Request) bool }

func (h hookAuth) Authenticate(r *http.Request) (billingauth.Verified, error) {
	if !h.admit(r) {
		return nil, billingauth.ErrUnauthenticated
	}
	return authtest.Verified{Grant: authtest.Grant{Identity: authtest.User("22222222-2222-4222-8222-222222222222")}}, nil
}

func mountAt(t *testing.T, rt *Engine, sel config.Routes, prefix string) *http.ServeMux {
	t.Helper()
	routes, err := rt.Routes(sel)
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+prefix+route.Path, route.Handler)
	}
	return mux
}

func serve(h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A caller's edits to returned routes never leak into another mount.
func TestRoutesCopyTheSelection(t *testing.T) {
	rt := httpRuntime()
	fake := &authtest.Fake{}
	routes, err := rt.Routes(config.Routes{Auth: fake})
	require.NoError(t, err)
	original := routes[0].Path
	routes[0].Path = "/caller-mutated"
	again, err := rt.Routes(config.Routes{Auth: fake})
	require.NoError(t, err)
	require.Equal(t, original, again[0].Path)

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { _, err := rt.Routes(config.Routes{Auth: fake}); require.NoError(t, err) })
	}
	wg.Wait()
}

func TestHTTPRouteExposureMatchesSelection(t *testing.T) {
	fake := &authtest.Fake{}
	routes, err := httpRuntime().Routes(config.Routes{Auth: fake})
	require.NoError(t, err)
	var public, customer bool
	for _, r := range routes {
		require.NotContains(t, r.Path, "/admin/", "without Permissions no admin or merchant-config route is mounted")
		public = public || r.Path == "/v1/catalog/products"
		customer = customer || strings.HasPrefix(r.Path, "/v1/me/")
	}
	require.True(t, public && customer, "the public and customer routes are always mounted")

	full := config.Routes{Auth: fake, RouteGroups: authtest.Groups(), Scope: authtest.Scope, Permissions: authtest.Permissions()}
	rt := httpRuntime()
	rt.App.Config.Vault = &config.VaultConfig{KVMount: "kv"}
	mux := mountAt(t, rt, full, "/api/pay")
	for _, tc := range []struct {
		method, path string
		code         int
	}{
		{http.MethodGet, "/api/pay/v1/me", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/me/checkout-sessions", http.StatusUnauthorized},
		{http.MethodPost, "/api/pay/v1/admin/psps", http.StatusUnauthorized},
		{http.MethodOptions, "/api/pay/v1/me", http.StatusNoContent},
		{http.MethodOptions, "/api/pay/v1/checkout-sessions/ocs_x/pay", http.StatusNoContent},
	} {
		rec := serve(mux, tc.method, tc.path, "{}", "Authorization", "Bearer invalid")
		require.Equal(t, tc.code, rec.Code, tc.method+" "+tc.path+" "+rec.Body.String())
	}
}

// The configuration's capabilities report what this mount serves.
func TestCapabilitiesReportTheMount(t *testing.T) {
	fake := &authtest.Fake{}
	read := func(sel config.Routes) map[string]bool {
		rec := serve(mountAt(t, httpRuntime(), sel, ""), http.MethodGet, "/v1/config", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var doc billing.PublicConfig
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
		require.NotEmpty(t, doc.Currencies)
		return doc.Capabilities.RouteGroups
	}
	require.Equal(t, map[string]bool{"admin": false, "catalog": false, "merchant_config": false, "metrics": false, "app": false}, read(config.Routes{Auth: fake}))
	require.Equal(t, map[string]bool{"admin": true, "catalog": false, "merchant_config": false, "metrics": false, "app": false}, read(config.Routes{Auth: fake, RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: config.Permissions{AdminRead: authtest.Perm(authtest.StaffRead)}}))
	require.Equal(t, map[string]bool{"admin": false, "catalog": false, "merchant_config": true, "metrics": false, "app": false}, read(config.Routes{Auth: fake, RouteGroups: config.RouteGroups{MerchantConfig: true}, Scope: authtest.Scope, Permissions: config.Permissions{MerchantConfig: authtest.Perm(authtest.StaffAdmin)}}))
	all := authtest.Groups()
	all.Programmatic = true
	require.Equal(t, map[string]bool{"admin": true, "catalog": true, "merchant_config": true, "metrics": true, "app": true}, read(config.Routes{Auth: fake, RouteGroups: all, Scope: authtest.Scope, Permissions: authtest.Permissions()}))
}

func TestCatalogEditsFollowTheCatalogGroup(t *testing.T) {
	writes := func(rt *Engine, sel config.Routes) (reads, writes int) {
		routes, err := rt.Routes(sel)
		require.NoError(t, err)
		for _, route := range routes {
			if !strings.Contains(route.Path, "/admin/catalog") || route.Method == http.MethodOptions {
				continue
			}
			// Offer lookup is a read with a POST body.
			if route.Method == http.MethodGet || route.Method == http.MethodHead || strings.HasSuffix(route.Path, "/offers/lookup") {
				reads++
			} else {
				writes++
			}
		}
		return reads, writes
	}
	fake := &authtest.Fake{}
	rt := httpRuntime()
	reads, n := writes(rt, config.Routes{Auth: fake, RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: authtest.AdminPermissions()})
	require.Zero(t, reads+n, "the catalog is its own group")
	mux := mountAt(t, rt, config.Routes{Auth: fake, RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: authtest.AdminPermissions()}, "/api/pay")
	for _, path := range []string{"/api/pay/v1/admin/catalog/products", "/api/pay/v1/admin/catalog/prices"} {
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, serve(mux, http.MethodPost, path, "").Code, path)
	}
	// Mounts choose independently; a host's catalog document never closes them.
	rt.App.Config.Catalog = &pkgcatalog.Application{}
	reads, n = writes(rt, config.Routes{Auth: fake, RouteGroups: config.RouteGroups{Catalog: true}, Scope: authtest.Scope, Permissions: config.Permissions{Catalog: authtest.Perm(authtest.StaffCatalog)}})
	require.Positive(t, reads)
	require.Positive(t, n)
	_, err := rt.Routes(config.Routes{Auth: fake})
	require.NoError(t, err)

	_, err = httpRuntime().Routes(config.Routes{RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: authtest.AdminPermissions()})
	require.ErrorContains(t, err, "Routes.Auth is required", "the admin API never mounts open")
	_, err = httpRuntime().Routes(config.Routes{Auth: (*authtest.Fake)(nil), RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: authtest.AdminPermissions()})
	require.ErrorContains(t, err, "Routes.Auth is required", "a typed nil is no Auth")
}

// A group on needs its permission, a permission needs its group on, and a
// typed nil permission is none.
func TestPermissionsFailClosed(t *testing.T) {
	fake := &authtest.Fake{}
	for name, tc := range map[string]struct {
		sel config.Routes
		err string
	}{
		"admin on without its read": {config.Routes{Auth: fake, RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: config.Permissions{AdminUpdate: authtest.Perm("w")}}, "RouteGroups.Admin is on without Permissions.AdminRead"},
		"a typed nil read":          {config.Routes{Auth: fake, RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: config.Permissions{AdminRead: (*nilPerm)(nil)}}, "RouteGroups.Admin is on without Permissions.AdminRead"},
		"a permission, group off":   {config.Routes{Auth: fake, Scope: authtest.Scope, Permissions: config.Permissions{Catalog: authtest.Perm("e")}}, "Permissions.Catalog is given, but RouteGroups.Catalog is off"},
	} {
		_, err := httpRuntime().Routes(tc.sel)
		require.ErrorContains(t, err, tc.err, name)
	}
}

type nilPerm struct{}

func (*nilPerm) String() string { panic("never called") }

func TestHTTPVerifierSeesOriginalSignedRequest(t *testing.T) {
	const target = "/api/pay/v1/me/invoices/inv%2F1?view=raw"
	const body = "{ \"signed\" : \"unaltered\" }\n"
	calls := 0
	verifier := hookAuth{admit: func(r *http.Request) bool {
		calls++
		require.Equal(t, target, r.RequestURI)
		require.Equal(t, "/api/pay/v1/me/invoices/inv%2F1", r.URL.RawPath)
		require.Equal(t, "inv/1", r.PathValue("id"))
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, body, string(raw))
		return false
	}}
	mux := mountAt(t, httpRuntime(), config.Routes{Auth: verifier}, "/api/pay")
	require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodGet, target, body, "Authorization", "Bearer signed").Code)
	require.Equal(t, 1, calls)

	// Routers that accept a trailing slash without redirecting still bind values.
	bound := routebundle.BindPathValues("/v1/tenants/{tenant}/me/invoices/{id}", http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		require.Equal(t, "acme", r.PathValue("tenant"))
		require.Equal(t, "invoice-1", r.PathValue("id"))
		require.Equal(t, "/api/pay/v1/tenants/acme/me/invoices/invoice-1/", r.URL.Path)
		calls++
	}))
	serve(bound, http.MethodGet, "/api/pay/v1/tenants/acme/me/invoices/invoice-1/", "")
	require.Equal(t, 2, calls)
}

// One limiter per runtime: a second host mount must not reset the counters.
func TestHTTPRateLimitIsSharedAcrossMountsAndRoutes(t *testing.T) {
	rt := httpRuntime()
	rt.App.Config.RateLimits = &config.RateLimitsConfig{"checkout": {RequestsPerMinute: 1}, "default": {RequestsPerMinute: 60}}
	sel := config.Routes{Auth: &authtest.Fake{}}
	first, second := mountAt(t, rt, sel, "/first"), mountAt(t, rt, sel, "/second")
	for i, tc := range []struct {
		mux  http.Handler
		path string
	}{
		{first, "/first/v1/me/checkout-sessions"},
		{second, "/second/v1/me/checkout-sessions"},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.94:1234"
		req.Header.Set("Authorization", "Bearer invalid")
		rec := httptest.NewRecorder()
		tc.mux.ServeHTTP(rec, req)
		want := http.StatusTooManyRequests
		if i == 0 {
			want = http.StatusUnauthorized
		}
		require.Equal(t, want, rec.Code, tc.path)
	}
}

// No route mounts without an Auth.
func TestRoutesNeedAuth(t *testing.T) {
	_, err := httpRuntime().Routes(config.Routes{})
	require.ErrorContains(t, err, "Routes.Auth is required", "customer routes never mount open")
}

// The customer routes are always mounted, the admin ones only with
// Permissions.
func TestCustomerRoutesAlwaysMounted(t *testing.T) {
	mux := mountAt(t, httpRuntime(), config.Routes{Auth: &authtest.Fake{}}, "/api/pay")
	for _, path := range []string{"/payments", "/invoices", "/subscriptions", "/payment-methods"} {
		require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodGet, "/api/pay/v1/me"+path, "").Code, path)
	}
	require.Equal(t, http.StatusUnauthorized, serve(mux, http.MethodPost, "/api/pay/v1/me/checkout-sessions", "").Code)
	require.Equal(t, http.StatusNotFound, serve(mux, http.MethodGet, "/api/pay/v1/admin/payments", "").Code)
	rec := serve(mux, http.MethodGet, "/api/pay/v1/config", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotContains(t, rec.Body.String(), `"routes"`)
}

// An ambient cookie never reaches the host's Auth: a browser call carries its
// credential in a header.
func TestAmbientCookiesNeverAuthenticate(t *testing.T) {
	calls := 0
	authn := hookAuth{admit: func(r *http.Request) bool {
		calls++
		_, err := r.Cookie("session")
		return err == nil
	}}
	mux := mountAt(t, httpRuntime(), config.Routes{Auth: authn}, "/api/pay")
	req := httptest.NewRequest(http.MethodPost, "/api/pay/v1/me/subscriptions/not-id/cancel", strings.NewReader("{}"))
	req.AddCookie(&http.Cookie{Name: "session", Value: "valid"})
	req.Header.Set("Origin", "https://portal.example")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	require.Equal(t, 1, calls)
}

// The admin console mounts with the admin API it drives, at the prefix's
// /admin, pointed at the API and the host's AuthKit on the same origin;
// otherwise Mount fails.
func TestAdminConsoleMountsWithTheAdminAPI(t *testing.T) {
	rt := httpRuntime()
	fake := &authtest.Fake{}
	_, err := rt.Routes(config.Routes{Prefix: "/billing", AdminConsole: true, Auth: fake})
	require.ErrorContains(t, err, "turn on at least one of RouteGroups.Admin")
	sel := config.Routes{Prefix: "/billing", RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: authtest.AdminPermissions(), AdminConsole: true, Auth: fake}
	_, err = rt.Routes(sel)
	require.ErrorContains(t, err, "needs a console build")
	rt.App.ConsoleAssets = fstest.MapFS{"index.html": {Data: []byte(`<!doctype html><base href="/admin/">host build`)}}
	_, err = rt.Routes(config.Routes{Prefix: "billing", Auth: fake, RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: authtest.AdminPermissions(), AdminConsole: true})
	require.Error(t, err)

	mux := mountAt(t, rt, sel, "")
	rec := serve(mux, http.MethodGet, "/billing/admin/", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `<base href="/billing/admin/">host build`)
	rec = serve(mux, http.MethodGet, "/billing/admin/config.json", "")
	require.JSONEq(t, `{"auth_base_url":"/api/v1","api_base_url":"/billing/v1","nl_widgets_enabled":false,"ask_enabled":false,"catalog_copilot_enabled":false,"catalog_drafting_enabled":false,"extensions":{},"issuer":null,"merchant":null}`, rec.Body.String())

	// An assistant shows where the deployment has an LLM and its group is mounted.
	for _, tc := range []struct {
		sel  config.Routes
		want string
	}{
		{sel, `"nl_widgets_enabled":false,"ask_enabled":false,"catalog_copilot_enabled":false`},
		{config.Routes{Prefix: "/billing", RouteGroups: authtest.Groups(), Scope: authtest.Scope, Permissions: authtest.Permissions(), AdminConsole: true, Auth: fake}, `"nl_widgets_enabled":true,"ask_enabled":true,"catalog_copilot_enabled":true`},
	} {
		assisted := httpRuntime()
		assisted.App.ConsoleAssets = rt.App.ConsoleAssets
		assisted.App.Config.LLM = &config.LLMConfig{APIKey: "k", AskEnabled: true, CatalogCopilotEnabled: true}
		require.Contains(t, serve(mountAt(t, assisted, tc.sel, ""), http.MethodGet, "/billing/admin/config.json", "").Body.String(), tc.want)
	}

	// The console acts for the merchant the engine serves.
	declared := httpRuntime()
	declared.App.ConsoleAssets = rt.App.ConsoleAssets
	declared.App.Config.Merchant = config.MerchantDeclaration{Slug: "acme", DisplayName: "Acme"}
	body := serve(mountAt(t, declared, sel, ""), http.MethodGet, "/billing/admin/config.json", "").Body.String()
	require.Contains(t, body, `"merchant":{"id":"11111111-1111-4111-8111-111111111111","slug":"acme","display_name":"Acme"}`)
	require.Equal(t, http.StatusOK, serve(mux, http.MethodGet, "/billing/v1/config", "").Code)

	off := mountAt(t, httpRuntime(), config.Routes{Prefix: "/billing", Auth: fake, RouteGroups: authtest.AdminGroups(), Scope: authtest.Scope, Permissions: authtest.AdminPermissions()}, "")
	require.Equal(t, http.StatusNotFound, serve(off, http.MethodGet, "/billing/admin/", "").Code, "no console unless selected")
}
