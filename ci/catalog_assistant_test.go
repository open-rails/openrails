//go:build e2e && integration

package ci_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/http/routes"
)

// Console ownership leaves the feature optional and merchant-authorized.
// Ordinary catalog endpoints remain available when assistance is disabled.
func TestCatalogAssistantIsAnOptionalConsoleRoute(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"assistant-" + uuid.NewString()[:8]: {}}
	cfg.LLM = &openrails.LLMConfig{APIKey: "test", CatalogCopilotEnabled: true}
	bill, client := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	model := &featureLimitLLM{}
	engine.Graph(bill).Runtime.CopilotService.SetLLM(model)
	auth := &authtest.Fake{}
	bound := authtest.MerchantScope(client.MerchantID())
	route, ok := routes.Lookup(http.MethodPost, "/v1/console/catalog/ask")
	require.True(t, ok)
	staff := auth.Issue(authtest.Grant{Identity: authtest.User(uuid.NewString()), Scope: bound, Bound: bound, Permissions: []billing.Permission{route.Needs().Merchant}})
	denied := auth.Issue(authtest.Grant{Identity: authtest.User(uuid.NewString()), Scope: bound, Bound: bound, Permissions: []billing.Permission{billing.MerchantCustomerRead}})
	mount := func(catalog bool) http.Handler {
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth, RouteGroups: openrails.RouteGroups{Catalog: catalog, Admin: true}}))
		return mux
	}
	api := mount(true)
	body := map[string]any{"question": "What do we sell?"}
	for _, token := range []string{"", denied} {
		got := send(api, token, http.MethodPost, route.Path, body)
		if token == "" {
			require.Equal(t, http.StatusUnauthorized, got.status, got.body)
		} else {
			require.Equal(t, http.StatusForbidden, got.status, got.body)
		}
		require.Zero(t, model.asks)
	}
	got := send(api, staff, http.MethodPost, route.Path, body)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.Contains(t, got.body, "No further lookups needed.")
	require.Equal(t, 1, model.asks)
	got = send(api, staff, http.MethodPost, "/v1/catalog/ask", body)
	require.Equal(t, http.StatusNotFound, got.status)
	got = send(api, denied, http.MethodGet, "/v1/catalog/products", nil)
	require.Equal(t, http.StatusForbidden, got.status, "assistant ownership grants no normal catalog authority")
	got = send(mount(false), staff, http.MethodPost, route.Path, body)
	require.Equal(t, http.StatusNotFound, got.status, "the catalog route group still controls mounting")

	engine.Graph(bill).Runtime.CopilotService.SetLLM(nil)
	got = send(api, staff, http.MethodPost, route.Path, body)
	require.Equal(t, http.StatusServiceUnavailable, got.status, "a configured mount still fails closed if its model disappears")
	disabled := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(disabled, bill, openrails.Routes{Prefix: "/disabled", Auth: auth, RouteGroups: openrails.RouteGroups{Catalog: true}}))
	got = send(disabled, staff, http.MethodPost, "/disabled"+route.Path, body)
	require.Equal(t, http.StatusNotFound, got.status, "an unconfigured model cannot expose the feature")
	got = send(disabled, staff, http.MethodGet, "/disabled/v1/catalog/products", nil)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.Equal(t, 1, model.asks)
}
