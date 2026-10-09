package embedhttp

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
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
		"solana_subscription_management": false, "provider_credential_writes": false,
		"api_host": false, "catalog_copilot": false, "metrics_ask": false, "dashboard_generation": false,
		"hosted_extra": true,
	}, caps.Features)

	// Credential writes need the merchant-config bundle.
	writable := routesurface.ProviderRoutes{SecretWrite: true}
	require.False(t, CapabilitiesFor(nil, httproutes.Permissions{AdminRead: "r", AdminUpdate: "w"}, false, writable, nil).Features["provider_credential_writes"])
	config := CapabilitiesFor(nil, httproutes.Permissions{MerchantConfig: "c"}, false, writable, nil)
	require.True(t, config.Features["provider_credential_writes"])
	require.Equal(t, map[string]bool{"admin": false, "catalog": false, "merchant_config": true, "metrics": false, "app": false}, config.RouteGroups)
}

// Route selection is refused unless each route group has the Auth and the
// permission it needs, and no permission is given for a group that is off:
// nothing mounts open.
func TestRoutesValidation(t *testing.T) {
	auth := &authtest.Fake{}
	read, update, catalog, admin, metrics := authtest.Perm("r"), authtest.Perm("u"), authtest.Perm("c"), authtest.Perm("a"), authtest.Perm("m")
	for _, tc := range []struct {
		name string
		sel  config.Routes
		err  string
	}{
		{"no Auth: the customer routes need it", config.Routes{}, "Routes.Auth is required"},
		{"a typed nil Auth", config.Routes{Auth: (*authtest.Fake)(nil)}, "Routes.Auth is required"},
		{"public, customer and webhooks", config.Routes{Auth: auth}, ""},
		{"admin reads", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read}}, ""},
		{"admin reads and updates", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: read, AdminUpdate: update}}, ""},
		{"admin on without its read", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminUpdate: update}}, "RouteGroups.Admin is on without Permissions.AdminRead"},
		{"a typed nil permission is none", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Admin: true}, Permissions: config.Permissions{AdminRead: (*authtest.Perm)(nil)}}, "without Permissions.AdminRead"},
		{"admin permissions with admin off", config.Routes{Auth: auth, Permissions: config.Permissions{AdminRead: read}}, "Permissions.AdminRead is given, but RouteGroups.Admin is off"},
		{"an update with admin off", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Catalog: true}, Permissions: config.Permissions{Catalog: catalog, AdminUpdate: update}}, "Permissions.AdminUpdate is given"},
		{"the catalog alone", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Catalog: true}, Permissions: config.Permissions{Catalog: catalog}}, ""},
		{"the catalog without its permission", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Catalog: true}}, "RouteGroups.Catalog is on without Permissions.Catalog"},
		{"merchant configuration alone", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{MerchantConfig: true}, Permissions: config.Permissions{MerchantConfig: admin}}, ""},
		{"metrics alone", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Metrics: true}, Permissions: config.Permissions{Metrics: metrics}}, ""},
		{"metrics given, off", config.Routes{Auth: auth, Permissions: config.Permissions{Metrics: metrics}}, "RouteGroups.Metrics is off"},
		{"programmatic needs no permission", config.Routes{Auth: auth, RouteGroups: config.RouteGroups{Programmatic: true}}, ""},
	} {
		err := ValidateRoutes(tc.sel)
		if tc.err == "" {
			require.NoError(t, err, tc.name)
		} else {
			require.ErrorContains(t, err, tc.err, tc.name)
		}
	}
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

// Provider credential writes need a DB secret backend that can write; an
// explicit route override can neither grant them nor invent a Solana signer.
func TestProviderRoutesForRuntime(t *testing.T) {
	for _, source := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, writable := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				rt := &app.Runtime{Config: &config.Config{SecretBackend: source}, RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: writable}}
				var override *routesurface.ProviderRoutes
				if explicit {
					all := routesurface.AllProviderRoutes()
					override = &all
				}
				routes := ProviderRoutesForRuntime(rt, override)
				require.Equal(t, source == config.SecretBackendDB && writable, routes.SecretWrite, "%s/%v/%v", source, writable, explicit)
				require.False(t, routes.SolanaSigning)
				require.True(t, routes.Webhooks)
				require.Equal(t, writable, rt.RouteCapabilities.SecretWrite, "route gating never mutates runtime capabilities")
			}
		}
	}
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
