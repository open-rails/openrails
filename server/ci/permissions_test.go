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
		{string(billing.MerchantCustomerRead), http.MethodGet, "/v1/admin/customers", "", http.StatusOK},
		{string(billing.MerchantConfigRead), http.MethodGet, "/v1/merchant/findings", "", http.StatusOK},
		{string(billing.MerchantCustomerManage), http.MethodPatch, "/v1/admin/customers/" + customer, `{}`, http.StatusOK},
		{string(billing.MerchantConfigManage), http.MethodPost, "/v1/merchant/psps/refresh", "", http.StatusAccepted},
		{string(billing.MerchantCatalogRead), http.MethodGet, "/v1/catalog/meters", "", http.StatusOK},
		{string(billing.MerchantConfigRead), http.MethodGet, "/v1/merchant/psps", "", http.StatusOK},
		{string(billing.MerchantMetricsRead), http.MethodGet, "/v1/metrics/schema", "", http.StatusOK},
		{string(billing.MerchantEntitlementsRead), http.MethodPost, "/v1/app/entitlements/check", `{"customer_id":"` + customer + `","entitlements":["content:any"]}`, http.StatusOK},
		{string(billing.MerchantCatalogRead), http.MethodGet, "/v1/catalog/products?entitlement=content:any", "", http.StatusOK},
		{string(billing.MerchantUsageManage), http.MethodPost, "/v1/app/admissions/release", `{"request_ids":["req-1"]}`, http.StatusOK},
		{string(billing.MerchantUsageRead), http.MethodGet, "/v1/app/running-charges/never-started", "", http.StatusNotFound},
		{string(billing.MerchantEventsRead), http.MethodGet, "/v1/app/events", "", http.StatusOK},
		{string(billing.MerchantOrdersManage), http.MethodPost, "/v1/app/orders/preview", `{"customer_id":"` + customer + `","lines":[{"price_id":"` + billing.PriceID("test:"+uuid.NewString()+":v1").String() + `"}]}`, http.StatusOK},
	}
	for _, route := range routes {
		w := send(handler, backend([]string{route.perm}), route.method, route.path, route.body)
		require.Equal(t, route.status, w.Code, "%s %s holding %s: %s", route.method, route.path, route.perm, w.Body.String())
		if route.status == http.StatusNotFound {
			require.Equal(t, "running_charge_not_found", errorCode(t, w), "past the gate, the handler's own answer")
		}

		others := slices.DeleteFunc(slices.Clone(every), func(p string) bool { return p == route.perm })
		w = send(handler, backend(others), route.method, route.path, route.body)
		require.Equal(t, http.StatusForbidden, w.Code, "%s %s with every permission but %s: %s", route.method, route.path, route.perm, w.Body.String())
		require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
	}

	// The former operator paths under /admin are removed, with no aliases.
	for _, old := range []string{"/v1/admin/findings", "/v1/admin/psps", "/v1/admin/catalog/products", "/v1/admin/configuration", "/v1/admin/price-migrations"} {
		w := send(handler, backend(every), http.MethodGet, old, "")
		require.Equal(t, http.StatusNotFound, w.Code, "retired operator path %s: %s", old, w.Body.String())
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
	require.Equal(t, http.StatusOK, send(handler, backend(every), http.MethodGet, "/v1/admin/customers", "").Code, "control: the admin group is on")
	require.Equal(t, http.StatusNotFound, send(handler, backend(every), http.MethodGet, "/v1/merchant/findings", "").Code, "findings require the merchant-config group")
}

// A person acts at a merchant only through a token bound to it. A root
// role's root: permissions reach every merchant through such a token, and a
// person's own sign-in reaches none. The openrails:merchant scope's ceiling
// names the root: permissions, so an exchanged token keeps them; a person
// holding none gains nothing from it.
func TestRootActsOnlyThroughABoundToken(t *testing.T) {
	f := newFixture(t)
	cp := f.newServer(t, resourced)
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
	supportSession, operatorSession := authtest.SignIn(t, ak, support).AccessToken, authtest.SignIn(t, ak, operator).AccessToken

	for _, session := range []string{operatorSession, supportSession} {
		w := call(t, handler, session, http.MethodGet, "/v1/admin/customers", nil)
		require.Equal(t, http.StatusForbidden, w.Code, "a person's own sign-in: %s", w.Body.String())
		require.Equal(t, billing.CodeMerchantUnresolved, errorCode(t, w))
	}

	for _, mid := range []billing.MerchantID{a, b} {
		bound := f.exchanged(t, handler, operatorSession, mid)
		for _, path := range []string{"/v1/admin/customers", "/v1/merchant/psps", "/v1/metrics/schema"} {
			w := bound.call(t, handler, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, w.Code, "the root holder reads %s at %s through its root: names: %s", path, mid, w.Body.String())
			require.Equal(t, mid.String(), w.Header().Get("OpenRails-Bound-Merchant"))
		}
		w := bound.call(t, handler, http.MethodGet, "/v1/admin/access", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, billing.AdminAccess{Admin: billing.AccessUpdate, Catalog: billing.AccessUpdate, MerchantConfig: billing.AccessUpdate, Metrics: true, ScopeID: mid.String(), ApplicationPermissions: []billing.Permission{}}, decode[billing.AdminAccess](t, w))
	}

	atA := f.exchanged(t, handler, supportSession, a)
	require.Equal(t, http.StatusOK, atA.call(t, handler, http.MethodGet, "/v1/admin/customers", nil).Code, "its own merchant")
	w := atA.call(t, handler, http.MethodGet, "/v1/merchant/psps", nil)
	require.Equal(t, http.StatusForbidden, w.Code, "the wider ceiling grants support nothing it does not hold: %s", w.Body.String())
	require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
	w = atA.call(t, handler, http.MethodGet, "/v1/admin/access", nil)
	require.Equal(t, billing.AdminAccess{Admin: billing.AccessUpdate, Catalog: billing.AccessNone, MerchantConfig: billing.AccessNone, ScopeID: a.String(), ApplicationPermissions: []billing.Permission{}}, decode[billing.AdminAccess](t, w))
	_, status, body := f.exchangeFor(t, handler, supportSession, b)
	require.Equal(t, http.StatusBadRequest, status, string(body))
	require.Contains(t, string(body), "invalid_target", "no role at b, no token bound to b")
}
