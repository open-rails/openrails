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
	caps := CapabilitiesFor(nil, httproutes.Permissions{AdminRead: "r"}, routesurface.ProviderRoutes{Solana: true}, map[string]bool{"hosted_extra": true})
	require.Equal(t, map[string]bool{"admin": true, "catalog_write": false, "merchant_config": false}, caps.RouteGroups)
	require.Equal(t, map[string]bool{
		"solana_one_time_payments": true, "stripe_billing_portal": false,
		"solana_subscription_management": false, "provider_credential_writes": false,
		"api_host": false, "catalog_copilot": false, "metrics_ask": false, "dashboard_generation": false,
		"hosted_extra": true,
	}, caps.Features)

	// Credential writes need the merchant-config bundle.
	writable := routesurface.ProviderRoutes{SecretWrite: true}
	require.False(t, CapabilitiesFor(nil, httproutes.Permissions{AdminRead: "r", AdminWrite: "w"}, writable, nil).Features["provider_credential_writes"])
	config := CapabilitiesFor(nil, httproutes.Permissions{MerchantConfig: "c"}, writable, nil)
	require.True(t, config.Features["provider_credential_writes"])
	require.Equal(t, map[string]bool{"admin": false, "catalog_write": false, "merchant_config": true}, config.RouteGroups)
}

// Route selection is refused unless each surface has the Auth it needs:
// nothing mounts open.
func TestRoutesValidation(t *testing.T) {
	auth := &authtest.Fake{}
	read, write, admin := authtest.Perm("r"), authtest.Perm("w"), authtest.Perm("a")
	customer := func(c config.CustomerRoutes) config.Routes {
		return config.Routes{Auth: auth, CustomerProfiles: []config.CustomerRoutes{c}}
	}
	for _, tc := range []struct {
		name string
		sel  config.Routes
		ok   bool
	}{
		{"no Auth: the customer routes need it", config.Routes{}, false},
		{"a typed nil Auth", config.Routes{Auth: (*authtest.Fake)(nil)}, false},
		{"public, customer and webhooks", config.Routes{Auth: auth}, true},
		{"admin reads", config.Routes{Auth: auth, Permissions: config.Permissions{AdminRead: read}}, true},
		{"admin reads and writes", config.Routes{Auth: auth, Permissions: config.Permissions{AdminRead: read, AdminWrite: write}}, true},
		{"admin writes without reads", config.Routes{Auth: auth, Permissions: config.Permissions{AdminWrite: write}}, false},
		{"a typed nil permission is none", config.Routes{Auth: auth, Permissions: config.Permissions{AdminRead: (*authtest.Perm)(nil), AdminWrite: write}}, false},
		{"merchant configuration alone", config.Routes{Auth: auth, Permissions: config.Permissions{MerchantConfig: admin}}, true},
		{"catalog edits with reads", config.Routes{Auth: auth, Permissions: config.Permissions{AdminRead: read, CatalogWrite: admin}}, true},
		{"catalog edits without reads", config.Routes{Auth: auth, Permissions: config.Permissions{CatalogWrite: admin}}, false},
		{"customer with the mount's Auth", customer(config.CustomerRoutes{Prefix: "/v1/shop/me"}), true},
		{"customer with its own Auth", customer(config.CustomerRoutes{Prefix: "/v1/shop/me", Auth: auth}), true},
		{"parameterized prefix", customer(config.CustomerRoutes{Prefix: "/v1/tenants/{tenant}/me"}), true},
	} {
		require.Equal(t, tc.ok, ValidateRoutes(tc.sel) == nil, tc.name)
	}
	for _, prefix := range []string{"/", "me", "/a/../b", "/a/", "/a/*", "/a b", "/a/{x.y}", "/a/b{c}", "/a/{}"} {
		require.Error(t, ValidateRoutes(customer(config.CustomerRoutes{Prefix: prefix})), prefix)
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
	require.Contains(t, keys, "GET /billing/v1/admin/config")
	require.Contains(t, keys, "OPTIONS /billing/v1/checkout-sessions/{id}/pay")
	require.Contains(t, keys, "GET /billing/v1/admin/payments")
	require.NotContains(t, keys, "OPTIONS /billing/v1/admin/payments")
	require.NotContains(t, keys, "POST /billing/v1/admin/payments/{id}/refunds", "writes need AdminWrite")
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
