//go:build e2e && integration

package ci_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// The standalone server's permissions are merchant:<resource>:<action>, one
// per Routes.Permissions field. Through the server's own handler, a
// client-credentials token holding exactly one passes that route's gate,
// one holding every other is refused it, and a server without
// route_groups.programmatic serves no /v1/app route.
func TestStandalonePermissionsArePersonaResourceAction(t *testing.T) {
	f := newFixture(t)
	host := newIssuerKey(t, "https://perms-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	shop := uniqueName("perms")
	trust := func(cfg *server.Config, _ *server.Deps) {
		cfg.ResourceServer = &server.ResourceServerConfig{
			Identifier: resourceID, DPoPNonceKey: strings.Repeat("n", 32),
			TrustedIssuers: []server.TrustedIssuerConfig{{Name: "host", Issuer: host.iss, Keys: host.pinned(t), Merchants: []string{shop}, Permissions: []string{"merchant:*"}}},
		}
	}
	cp := f.newServer(t, trust)
	provision(t, cp, shop)
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	every := []string{
		server.MerchantBillingRead, server.MerchantBillingManage, server.MerchantCatalogManage, server.MerchantConfigManage, server.MerchantMetricsRead,
		server.MerchantEntitlementsRead, server.MerchantCatalogRead, server.MerchantUsageManage, server.MerchantCostsManage, server.MerchantEventsRead,
	}
	require.Equal(t, []string{
		"merchant:billing:read", "merchant:billing:manage", "merchant:catalog:manage", "merchant:config:manage", "merchant:metrics:read",
		"merchant:entitlements:read", "merchant:catalog:read", "merchant:usage:manage", "merchant:costs:manage", "merchant:events:read",
	}, every)
	backend := func(perms []string) string {
		return host.mint(t, func(c jwt.MapClaims) { c["sub"], c["client_id"], c["permissions"] = "backend", "backend", perms })
	}
	send := func(h http.Handler, token, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if method != http.MethodGet && strings.HasPrefix(path, "/v1/app/") {
			r.Header.Set("Idempotency-Key", uuid.NewString())
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	customer := billing.CustomerID(uuid.New()).String()
	routes := []struct {
		perm, method, path, body string
		status                   int
	}{
		{server.MerchantBillingRead, http.MethodGet, "/v1/admin/findings", "", http.StatusOK},
		{server.MerchantBillingManage, http.MethodPost, "/v1/admin/psps/refresh", "", http.StatusAccepted},
		{server.MerchantCatalogManage, http.MethodGet, "/v1/admin/catalog/revision", "", http.StatusOK},
		{server.MerchantConfigManage, http.MethodGet, "/v1/admin/psps", "", http.StatusOK},
		{server.MerchantMetricsRead, http.MethodGet, "/v1/admin/metrics/schema", "", http.StatusOK},
		{server.MerchantEntitlementsRead, http.MethodPost, "/v1/app/entitlements/check", `{"customer_id":"` + customer + `","entitlements":["content:any"]}`, http.StatusOK},
		{server.MerchantCatalogRead, http.MethodGet, "/v1/app/catalog/products?entitlement=content:any", "", http.StatusOK},
		{server.MerchantUsageManage, http.MethodPost, "/v1/app/admissions/release", `{"request_ids":["req-1"]}`, http.StatusOK},
		{server.MerchantCostsManage, http.MethodPost, "/v1/app/provider-operations/op-missing/release", `{"release_reference":"never-opened"}`, http.StatusNotFound},
		{server.MerchantEventsRead, http.MethodGet, "/v1/app/host-events", "", http.StatusOK},
	}
	for _, route := range routes {
		w := send(handler, backend([]string{route.perm}), route.method, route.path, route.body)
		require.Equal(t, route.status, w.Code, "%s %s holding %s: %s", route.method, route.path, route.perm, w.Body.String())
		if route.status == http.StatusNotFound {
			require.Equal(t, "provider_operation_not_found", errorCode(t, w), "past the gate, the handler's own answer")
		}

		others := slices.DeleteFunc(slices.Clone(every), func(p string) bool { return p == route.perm })
		w = send(handler, backend(others), route.method, route.path, route.body)
		require.Equal(t, http.StatusForbidden, w.Code, "%s %s with every permission but %s: %s", route.method, route.path, route.perm, w.Body.String())
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
	}

	// Without route_groups.programmatic, /v1/app is not served at all.
	other := newFixture(t)
	staffOnly := other.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		trust(cfg, deps)
		cfg.RouteGroups = openrails.RouteGroups{Admin: true}
	})
	provision(t, staffOnly, shop)
	handler, err = standaloneHandler(staffOnly)
	require.NoError(t, err)
	for _, route := range routes {
		if !strings.HasPrefix(route.path, "/v1/app/") {
			continue
		}
		w := send(handler, backend(every), route.method, route.path, route.body)
		require.Equal(t, http.StatusNotFound, w.Code, "%s %s without programmatic: %s", route.method, route.path, w.Body.String())
	}
	require.Equal(t, http.StatusOK, send(handler, backend(every), http.MethodGet, "/v1/admin/findings", "").Code, "control: the admin group is on")
}
