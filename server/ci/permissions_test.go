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

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// The standalone server's permissions are OpenRails' own, each the full
// persona:resource:action: its role catalog holds exactly
// billing.Permissions, merchant: on the merchant persona and root: on root,
// the operator's root permissions and AuthKit's built-ins. Through the
// server's own handler, a client-credentials token holding exactly one
// merchant: name passes that route's gate, one holding every other is
// refused it, and a server without route_groups.programmatic serves no
// /v1/app route.
func TestStandalonePermissionsArePersonaResourceAction(t *testing.T) {
	f := newFixture(t)
	host := newIssuerKey(t, "https://perms-"+strings.ReplaceAll(f.schema, "_", "-")+".e2e.test")
	shop := uniqueName("perms")
	resource := func(cfg *server.Config, _ *server.Deps) {
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
	}
	cp := f.newServer(t, resource)
	mid := provision(t, cp, shop)
	trust(t, cp, mid, host.app(t, "owner", nil))
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	// The running catalog: the merchant owner's role lists its persona's,
	// and root's is the server's declaration. AuthKit's built-ins are what
	// the same personas hold with nothing declared.
	var catalog []string
	roles, err := cp.AuthKit().ListGroupRoles(t.Context(), iam.GroupByID(mid.String()))
	require.NoError(t, err)
	for _, role := range roles {
		if role.Name == controlplane.MerchantOwner {
			for _, perm := range role.Permissions {
				catalog = append(catalog, perm.String())
			}
		}
	}
	for _, perm := range controlplane.Roles.Root.Permissions() {
		catalog = append(catalog, perm.String())
	}
	bare := authkit.NewRoles()
	want := []string{}
	for _, perm := range append(bare.Root.Permissions(), bare.Persona(billing.MerchantGroupPersona, authkit.APIKeys, authkit.RemoteApplications, authkit.CustomRoles, authkit.OAuthClients).Permissions()...) {
		want = append(want, perm.String())
	}
	require.NotEmpty(t, want)
	var every []string
	for _, perm := range append(billing.Permissions(), billing.RootMerchantsRead, billing.RootMerchantsDelete, billing.RootMerchantsRestore, billing.RootWorkerHealthRead, billing.RootAdminRateLimitsUnlock) {
		want = append(want, string(perm))
		if perm.Persona() == billing.MerchantGroupPersona {
			every = append(every, string(perm))
		}
	}
	require.ElementsMatch(t, want, catalog)
	for _, perm := range catalog {
		require.Len(t, strings.Split(perm, ":"), 3, "%s is persona:resource:action", perm)
	}
	for _, role := range server.MerchantRoles() {
		grants, err := cp.AuthKit().RolePermissions(role)
		require.NoError(t, err)
		for _, grant := range grants {
			if role == controlplane.MerchantOwner {
				require.Equal(t, "merchant:*", grant.String())
				continue
			}
			require.Contains(t, billing.Permissions(), billing.Permission(grant.String()), "%s grants only OpenRails' own names", role)
		}
	}
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
		{string(billing.MerchantBillingRead), http.MethodGet, "/v1/admin/findings", "", http.StatusOK},
		{string(billing.MerchantBillingManage), http.MethodPatch, "/v1/admin/customers/" + customer, `{}`, http.StatusOK},
		{string(billing.MerchantConfigManage), http.MethodPost, "/v1/admin/psps/refresh", "", http.StatusAccepted},
		{string(billing.MerchantCatalogManage), http.MethodGet, "/v1/admin/catalog/revision", "", http.StatusOK},
		{string(billing.MerchantConfigManage), http.MethodGet, "/v1/admin/psps", "", http.StatusOK},
		{string(billing.MerchantMetricsRead), http.MethodGet, "/v1/admin/metrics/schema", "", http.StatusOK},
		{string(billing.MerchantEntitlementsRead), http.MethodPost, "/v1/app/entitlements/check", `{"customer_id":"` + customer + `","entitlements":["content:any"]}`, http.StatusOK},
		{string(billing.MerchantCatalogRead), http.MethodGet, "/v1/app/catalog/products?entitlement=content:any", "", http.StatusOK},
		{string(billing.MerchantUsageManage), http.MethodPost, "/v1/app/admissions/release", `{"request_ids":["req-1"]}`, http.StatusOK},
		{string(billing.MerchantCostsManage), http.MethodPost, "/v1/app/provider-operations/op-missing/release", `{"release_reference":"never-opened"}`, http.StatusNotFound},
		{string(billing.MerchantEventsRead), http.MethodGet, "/v1/app/host-events", "", http.StatusOK},
		{string(billing.MerchantOrdersManage), http.MethodPost, "/v1/app/orders/preview", `{"customer_id":"` + customer + `","lines":[{"price_id":"` + billing.PriceID(uuid.New()).String() + `"}]}`, http.StatusOK},
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
		resource(cfg, deps)
		cfg.RouteGroups = openrails.RouteGroups{Admin: true}
	})
	trust(t, staffOnly, provision(t, staffOnly, shop), host.app(t, "owner", nil))
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

// A root role's root: permissions reach every merchant, the merchant named
// by the request; a merchant's own staff hold the merchant: names in its
// group and act at that merchant alone.
func TestRootHoldersActAtAnyMerchant(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, nil)
	ak := cp.AuthKit()
	a, b := provision(t, cp, uniqueName("shop-a")), provision(t, cp, uniqueName("shop-b"))
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)

	support, operator := newAccount(t, cp), newAccount(t, cp)
	role, ok := server.MerchantRole("support")
	require.True(t, ok)
	authtest.GrantRole(t, ak, iam.GroupByID(a.String()), iam.UserSubject(support.ID), role)
	authtest.EnrollTOTP(t, ak, operator) // a root role needs a second factor
	authtest.GrantRole(t, ak, iam.RootGroup(), iam.UserSubject(operator.ID), controlplane.Roles.Root.Owner)
	supportToken, operatorToken := authtest.SignIn(t, ak, support).AccessToken, authtest.SignIn(t, ak, operator).AccessToken

	at := func(token string, mid billing.MerchantID, method, path string) *httptest.ResponseRecorder {
		return call(t, handler, token, method, path, "id:"+mid.String(), nil)
	}
	require.Equal(t, http.StatusOK, at(supportToken, a, http.MethodGet, "/v1/admin/customers").Code, "its own merchant")
	for _, path := range []string{"/v1/admin/customers", "/v1/admin/psps"} {
		w := at(supportToken, b, http.MethodGet, path)
		require.Equal(t, http.StatusForbidden, w.Code, "%s at another merchant: %s", path, w.Body.String())
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
	}
	w := at(supportToken, a, http.MethodGet, "/v1/admin/psps")
	require.Equal(t, http.StatusForbidden, w.Code, "support holds no merchant:config:manage: %s", w.Body.String())

	for _, mid := range []billing.MerchantID{a, b} {
		for _, path := range []string{"/v1/admin/customers", "/v1/admin/psps", "/v1/admin/metrics/schema"} {
			w := at(operatorToken, mid, http.MethodGet, path)
			require.Equal(t, http.StatusOK, w.Code, "the root holder reads %s at %s: %s", path, mid, w.Body.String())
		}
	}

	// The access read answers what each holds at the merchant named.
	access := func(token string, mid billing.MerchantID) billing.AdminAccess {
		w := at(token, mid, http.MethodGet, "/v1/admin/access")
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return decode[billing.AdminAccess](t, w)
	}
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessUpdate, ScopeID: a.String()}, access(supportToken, a))
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessNone, ScopeID: b.String()}, access(supportToken, b))
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessUpdate, Catalog: true, MerchantConfig: true, Metrics: true, ScopeID: b.String()}, access(operatorToken, b))
}
