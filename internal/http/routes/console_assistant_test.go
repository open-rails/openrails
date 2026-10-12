package routes

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

func TestConsoleOwnershipCannotHideMerchantOperations(t *testing.T) {
	route, ok := Lookup(POST, "/v1/console/catalog/ask")
	require.True(t, ok)
	require.True(t, route.ConsoleOnly)
	require.False(t, route.ClientMethod())
	require.Empty(t, route.Name)
	require.NoError(t, route.consoleOwnership())
	for name, change := range map[string]func(*Route){
		"ordinary path":      func(r *Route) { r.Path = "/v1/catalog/products" },
		"missing annotation": func(r *Route) { r.ConsoleOnly = false },
		"always mounted":     func(r *Route) { r.When = Always },
		"another feature":    func(r *Route) { r.When = FeatureSolana },
		"public":             func(r *Route) { r.Auth = AuthPublic },
		"another group":      func(r *Route) { r.Group = Admin },
		"mutation":           func(r *Route) { r.Level = LevelUpdate },
		"SDK method":         func(r *Route) { r.Name = "AskCatalog" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := route
			change(&changed)
			require.Error(t, changed.consoleOwnership())
		})
	}
	products, ok := Lookup(GET, "/v1/catalog/products")
	require.True(t, ok)
	require.True(t, products.ClientMethod())
	products.ConsoleOnly = true
	require.Error(t, products.consoleOwnership(), "ordinary catalog routes cannot opt out of SDK coverage")
}

func TestCatalogAssistantRetainsCatalogAuthority(t *testing.T) {
	rt := gatedRuntime(t)
	route, ok := Lookup(POST, "/v1/console/catalog/ask")
	require.True(t, ok)
	person := authtest.User(userA)
	allowed := &recordingAuth{who: person, allowed: map[billing.Permission]bool{route.Needs().Root: true}}
	got := runRoute(t, rt, allowed, route.Key(), nil)
	require.Equal(t, http.StatusNoContent, got.status)
	require.Equal(t, merchantA, got.staff.Merchant)
	denied := &recordingAuth{who: person, allowed: map[billing.Permission]bool{billing.RootCustomerRead: true}}
	for _, key := range []string{route.Key(), "GET /v1/catalog/products"} {
		got := runRoute(t, rt, denied, key, nil)
		require.Equal(t, http.StatusForbidden, got.status)
		require.Equal(t, billing.CodePermissionRequired, got.code)
		require.Zero(t, got.staff)
	}
	_, old := Lookup(POST, "/v1/catalog/ask")
	require.False(t, old, "no merchant-API compatibility alias")
}
