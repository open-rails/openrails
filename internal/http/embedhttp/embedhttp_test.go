package embedhttp

import (
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
)

func TestCapabilities(t *testing.T) {
	caps := CapabilitiesFor(nil, httproutes.Permissions{AdminRead: "r"}, true, routesurface.ProviderRoutes{Solana: true}, map[string]bool{"hosted_extra": true})
	require.Equal(t, map[string]bool{"admin": true, "catalog": false, "merchant_config": false, "metrics": false, "app": true}, caps.RouteGroups)
	require.Equal(t, map[string]bool{
		"solana_one_time_payments": true, "stripe_billing_portal": false,
		"solana_subscription_management": false, "merchant_config_edits": false,
		"api_host": false, "catalog_copilot": false, "metrics_ask": false, "dashboard_generation": false,
		"hosted_extra": true,
	}, caps.Features)

	// Merchant-config edits need the group and an editable source.
	editable := &app.Runtime{Config: &config.Config{Vault: &config.VaultConfig{KVMount: "kv"}}}
	require.False(t, CapabilitiesFor(editable, httproutes.Permissions{AdminRead: "r", AdminUpdate: "w"}, false, routesurface.ProviderRoutes{}, nil).Features["merchant_config_edits"])
	require.False(t, CapabilitiesFor(&app.Runtime{Config: &config.Config{}}, httproutes.Permissions{MerchantConfig: "c"}, false, routesurface.ProviderRoutes{}, nil).Features["merchant_config_edits"])
	config := CapabilitiesFor(editable, httproutes.Permissions{MerchantConfig: "c"}, false, routesurface.ProviderRoutes{}, nil)
	require.True(t, config.Features["merchant_config_edits"])
	require.Equal(t, map[string]bool{"admin": false, "catalog": false, "merchant_config": true, "metrics": false, "app": false}, config.RouteGroups)
}

// Route selection is refused unless each route group has the Auth and the
// permission it needs, and no permission is given for a group that is off:
// nothing mounts open.
func TestRoutesValidation(t *testing.T) {
	auth := &authtest.Fake{}
	read, update, catalog, admin, metrics := authtest.Perm("r"), authtest.Perm("u"), authtest.Perm("c"), authtest.Perm("a"), authtest.Perm("m")
	usage, events := authtest.Perm("g"), authtest.Perm("e")
	programmatic := config.RouteGroups{Programmatic: true}
	for _, tc := range []struct {
		name string
		sel  config.Routes
		err  string
	}{
		{"no Auth: the customer routes need it", config.Routes{}, "Routes.Auth is required"},
		{"a typed nil Auth", config.Routes{Auth: (*authtest.Fake)(nil)}, "Routes.Auth is required"},
		{"public, customer and webhooks", config.Routes{Auth: auth}, ""},
		{"admin reads", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read}}, ""},
		{"admin reads and updates", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read, AdminUpdate: update}}, ""},
		{"admin on without its read", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminUpdate: update}}, "RouteGroups.Admin is on without Permissions.AdminRead"},
		{"a typed nil permission is none", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: (*authtest.Perm)(nil)}}, "without Permissions.AdminRead"},
		{"admin permissions with admin off", config.Routes{Auth: auth, Permissions: config.Permissions{AdminRead: read}}, "Permissions.AdminRead is given, but RouteGroups.Admin is off"},
		{"an update with admin off", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Catalog: true}, Permissions: config.Permissions{Catalog: catalog, AdminUpdate: update}}, "Permissions.AdminUpdate is given"},
		{"the catalog alone", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Catalog: true}, Permissions: config.Permissions{Catalog: catalog}}, ""},
		{"the catalog without its permission", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Catalog: true}}, "RouteGroups.Catalog is on without Permissions.Catalog"},
		{"merchant configuration alone", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{MerchantConfig: true}, Permissions: config.Permissions{MerchantConfig: admin}}, ""},
		{"metrics alone", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Metrics: true}, Permissions: config.Permissions{Metrics: metrics}}, ""},
		{"metrics given, off", config.Routes{Auth: auth, Permissions: config.Permissions{Metrics: metrics}}, "RouteGroups.Metrics is off"},
		{"programmatic with SCIM alone", config.Routes{Auth: auth, RouteGroups: programmatic}, ""},
		{"programmatic usage", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: programmatic, Permissions: config.Permissions{Usage: usage}}, ""},
		{"programmatic usage and events", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: programmatic, Permissions: config.Permissions{Usage: usage, Events: events}}, ""},
		{"a programmatic permission without Scope", config.Routes{Auth: auth, RouteGroups: programmatic, Permissions: config.Permissions{Events: events}}, "without Routes.Scope"},
		{"a programmatic permission, programmatic off", config.Routes{Auth: auth, Scope: authtest.Scope, Permissions: config.Permissions{Usage: usage}}, "Permissions.Usage is given, but RouteGroups.Programmatic is off"},
		{"a programmatic permission with only staff on", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read, Events: events}}, "Permissions.Events is given, but RouteGroups.Programmatic is off"},
		{"a programmatic permission the Auth does not know", config.Routes{Auth: catalogued{auth}, Scope: authtest.Scope, RouteGroups: programmatic, Permissions: config.Permissions{Costs: authtest.Perm("misspelled")}}, `does not know the permission "misspelled"`},
		{"a staff group without Scope", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read}}, "without Routes.Scope"},
		{"a Scope without its ID", config.Routes{Auth: auth, Scope: billingauth.Scope{Authority: "test"}, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read}}, "without Routes.Scope"},
		{"a Scope without a permission", config.Routes{Auth: auth, Scope: authtest.Scope, RouteGroups: programmatic}, "Routes.Scope is given, but no permission is"},
		{"a permission the Auth does not know", config.Routes{Auth: catalogued{auth}, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: authtest.Perm("misspelled")}}, `does not know the permission "misspelled"`},
		{"permissions the Auth knows", config.Routes{Auth: catalogued{auth}, Scope: authtest.Scope, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read}}, ""},
	} {
		err := ValidateRoutes(tc.sel)
		if tc.err == "" {
			require.NoError(t, err, tc.name)
		} else {
			require.ErrorContains(t, err, tc.err, tc.name)
		}
	}
}

// catalogued knows the permissions r, u, c, a, m, g and e.
type catalogued struct{ *authtest.Fake }

func (catalogued) KnownPermission(p string) bool {
	return strings.Contains("rucamge", p) && len(p) == 1
}

// The combined handler refuses to mount the admin API without its Auth,
// and only the configuration and checkout routes join the permissive-CORS
// browser tier.
func TestNewRoutes(t *testing.T) {
	require.Panics(t, func() { (&Assembler{}).NewRoutes(Options{Permissions: httproutes.Permissions{AdminRead: "r"}}) })

	asm := &Assembler{Runtime: &app.Runtime{Config: &config.Config{}}, Auth: &authtest.Fake{}}
	noWebhooks := routesurface.ProviderRoutes{}
	table := asm.NewRoutes(Options{Permissions: httproutes.Permissions{AdminRead: "r"}, ProviderRoutes: &noWebhooks})
	var keys []string
	for _, e := range table.Entries {
		key := e.Method + " " + e.Path
		keys = append(keys, key)
		require.Equal(t, strings.HasPrefix(e.Path, "/billing/v1/checkout") || strings.HasPrefix(e.Path, "/billing/v1/captcha") ||
			slices.Contains([]string{"/billing/v1/catalog/products", "/billing/v1/config"}, e.Path), e.Browser, key)
		require.False(t, strings.HasPrefix(e.Path, "/billing/v1/webhooks/"), "callbacks need a webhook-capable rail")
	}
	require.Contains(t, keys, "GET /billing/v1/config")
	require.Contains(t, keys, "OPTIONS /billing/v1/checkout-sessions/{id}/pay")
	require.Contains(t, keys, "GET /billing/v1/admin/payments")
	require.NotContains(t, keys, "OPTIONS /billing/v1/admin/payments")
	require.NotContains(t, keys, "POST /billing/v1/admin/payments/{id}/refunds", "writes need AdminUpdate")
	require.NotContains(t, keys, "GET /billing/v1/admin/psps", "configuration needs MerchantConfig")
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

// Every Need names a field of the routes' Permissions and of the host's
// config.Permissions, and every field of either is a Need: a new programmatic
// permission (AppNeeds) is added in each, or this names what is missing.
func TestEveryNeedIsAField(t *testing.T) {
	needs := map[string]bool{}
	for _, n := range httproutes.AllNeeds() {
		needs[string(n)] = true
		require.NotNil(t, (&httproutes.Permissions{}).Field(n), "routes.Permissions.Field(%s)", n)
		f, ok := reflect.TypeOf(config.Permissions{}).FieldByName(string(n))
		require.True(t, ok, "config.Permissions has no field %s", n)
		require.Equal(t, reflect.TypeOf((*fmt.Stringer)(nil)).Elem(), f.Type, "config.Permissions.%s", n)
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(httproutes.Permissions{}), reflect.TypeOf(config.Permissions{})} {
		for i := range typ.NumField() {
			require.True(t, needs[typ.Field(i).Name], "%s.%s is no Need", typ, typ.Field(i).Name)
		}
	}
}
