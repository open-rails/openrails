package embedhttp

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

func TestCapabilities(t *testing.T) {
	caps := CapabilitiesFor(nil, config.RouteGroups{Admin: true, Programmatic: true}, routesurface.ProviderRoutes{Solana: true}, map[string]bool{"hosted_extra": true})
	require.Equal(t, map[string]bool{"admin": true, "catalog": false, "merchant_config": false, "metrics": false, "app": true}, caps.RouteGroups)
	require.Equal(t, map[string]bool{
		"solana_one_time_payments": true, "stripe_billing_portal": false,
		"solana_subscription_management": false, "merchant_config_edits": false,
		"catalog_copilot": false,
		"hosted_extra":    true, "customer_provisioning": false,
	}, caps.Features)

	// Merchant-config edits need the group and an editable source.
	editable := &app.Runtime{Config: &config.Config{Vault: &config.VaultConfig{KVMount: "kv"}}}
	require.False(t, CapabilitiesFor(editable, config.RouteGroups{Admin: true}, routesurface.ProviderRoutes{}, nil).Features["merchant_config_edits"])
	require.False(t, CapabilitiesFor(&app.Runtime{Config: &config.Config{}}, config.RouteGroups{MerchantConfig: true}, routesurface.ProviderRoutes{}, nil).Features["merchant_config_edits"])
	config := CapabilitiesFor(editable, config.RouteGroups{MerchantConfig: true}, routesurface.ProviderRoutes{}, nil)
	require.True(t, config.Features["merchant_config_edits"])
	require.Equal(t, map[string]bool{"admin": false, "catalog": false, "merchant_config": true, "metrics": false, "app": false}, config.RouteGroups)
}

// Route selection is refused without the Auth its routes need: any route
// needs one, a staff or programmatic group one with a root scope. Nothing
// mounts open.
func TestRoutesValidation(t *testing.T) {
	auth := &authtest.Fake{}
	for _, tc := range []struct {
		name string
		sel  config.Routes
		err  string
	}{
		{"no Auth: the customer routes need it", config.Routes{}, "Routes.Auth is required"},
		{"a typed nil Auth", config.Routes{Auth: (*authtest.Fake)(nil)}, "Routes.Auth is required"},
		{"public, customer and webhooks", config.Routes{Auth: auth}, ""},
		{"public, customer and webhooks, no root scope", config.Routes{Auth: rootless{auth}}, ""},
		{"admin", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Admin: true}}, ""},
		{"every group", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}}, ""},
		{"a staff group, no root scope", config.Routes{Auth: rootless{auth}, RouteGroups: config.RouteGroups{Metrics: true}}, "authkit/auth Root"},
		{"programmatic, no root scope", config.Routes{Auth: rootless{auth}, RouteGroups: config.RouteGroups{Programmatic: true}}, "authkit/auth Root"},
	} {
		err := ValidateRoutes(tc.sel)
		if tc.err == "" {
			require.NoError(t, err, tc.name)
		} else {
			require.ErrorContains(t, err, tc.err, tc.name)
		}
	}
}

// rootless is an Authenticator that is no authkit/auth Root.
type rootless struct{ a *authtest.Fake }

func (r rootless) Authenticate(req *http.Request) (billingauth.Verified, error) {
	return r.a.Authenticate(req)
}

// catalogued knows only its own permissions (authkit/auth PermissionCatalog).
type catalogued struct {
	*authtest.Fake
	known []billing.Permission
}

func (c catalogued) KnownPermission(p string) bool {
	return slices.Contains(c.known, billing.Permission(p))
}

// The combined handler refuses to mount the admin API without its Auth,
// and only the configuration and checkout routes join the permissive-CORS
// browser tier. A group that is on mounts its reads and its changes.
func TestNewRoutes(t *testing.T) {
	admin := config.RouteGroups{Admin: true}
	require.Panics(t, func() { (&Assembler{}).NewRoutes(Options{Groups: admin}) })

	asm := &Assembler{Runtime: &app.Runtime{Config: &config.Config{}}, Auth: &authtest.Fake{}}
	noWebhooks := routesurface.ProviderRoutes{}
	table := asm.NewRoutes(Options{Groups: admin, ProviderRoutes: &noWebhooks})
	var keys []string
	for _, e := range table.Entries {
		key := e.Method + " " + e.Path
		keys = append(keys, key)
		require.Equal(t, strings.HasPrefix(e.Path, "/billing/v1/checkout") || strings.HasPrefix(e.Path, "/billing/v1/captcha") ||
			slices.Contains([]string{"/billing/v1/products", "/billing/v1/config"}, e.Path), e.Browser, key)
		require.False(t, strings.HasPrefix(e.Path, "/billing/v1/webhooks/"), "callbacks need a webhook-capable rail")
	}
	require.Contains(t, keys, "GET /billing/v1/config")
	require.Contains(t, keys, "OPTIONS /billing/v1/config")
	require.Contains(t, keys, "GET /billing/v1/admin/payments")
	require.NotContains(t, keys, "OPTIONS /billing/v1/admin/payments")
	require.Contains(t, keys, "POST /billing/v1/admin/payments/{id}/refunds", "the admin group mounts its changes")
	require.NotContains(t, keys, "GET /billing/v1/merchant/psps", "configuration needs MerchantConfig")
}

// A route whose permissions the Auth knows neither of is not mounted, and
// the mount fails naming it; one name is enough.
func TestMountNeedsAKnownPermission(t *testing.T) {
	rt := &app.Runtime{Config: &config.Config{}}
	mount := func(known ...billing.Permission) (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = v.(httproutes.MountError)
			}
		}()
		(&Assembler{Runtime: rt, Auth: catalogued{Fake: &authtest.Fake{}, known: known}}).NewRoutes(Options{Groups: config.RouteGroups{Admin: true}})
		return nil
	}
	require.NoError(t, mount(billing.RootCustomerRead, billing.RootCustomerManage), "the root: names alone")
	require.NoError(t, mount(billing.MerchantCustomerRead, billing.MerchantCustomerManage), "the merchant: names alone")
	require.NoError(t, mount(billing.MerchantCustomerRead, billing.RootCustomerManage))

	err := mount(billing.RootCustomerRead)
	require.ErrorContains(t, err, string(billing.MerchantCustomerManage))
	require.ErrorContains(t, err, string(billing.RootCustomerManage))
	var mountErr httproutes.MountError
	require.ErrorAs(t, err, &mountErr)
	method, path, _ := strings.Cut(mountErr.Route, " ")
	route, ok := httproutes.Lookup(method, path)
	require.True(t, ok, "the error names the route: %q", mountErr.Route)
	require.Equal(t, httproutes.LevelUpdate, route.Level)
}

// An explicit route selection wins; without one an unbound runtime mounts
// every provider route and each request checks its own account.
func TestProviderRoutesForRuntime(t *testing.T) {
	only := routesurface.ProviderRoutes{Webhooks: true}
	require.Equal(t, only, ProviderRoutesForRuntime(&app.Runtime{Config: &config.Config{}}, &only))
	require.Equal(t, routesurface.AllProviderRoutes(), ProviderRoutesForRuntime(&app.Runtime{Config: &config.Config{}}, nil))
}

// Native adapters mirror gin's tree: catch-all subtrees own their descendants
// and one wildcard name per position.
func TestValidateRouteTable(t *testing.T) {
	h := http.NotFoundHandler()
	entry := func(method, path string) router.Entry { return router.Entry{Method: method, Path: path, Handler: h} }
	console, customer := entry("GET", "/admin/{asset...}"), entry("GET", "/admin/custom/balance")
	for _, entries := range [][]router.Entry{
		{console, customer}, {customer, console},
		{entry("GET", "/a/{id}/x"), entry("GET", "/a/{name}/y")},
		{entry("GET", "/dup"), entry("GET", "/dup")},
	} {
		require.Error(t, ValidateRouteTable(&router.Table{Entries: entries}), "%v", entries)
	}
	require.NoError(t, ValidateRouteTable(&router.Table{Entries: []router.Entry{
		console, entry("GET", "/admin"), entry("POST", "/admin/custom/subscriptions/{id}/cancel"),
		entry("GET", "/a/{id}/x"), entry("POST", "/a/{name}/y"),
	}}))
}
