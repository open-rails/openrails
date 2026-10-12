//go:build e2e && integration

package ci_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// consoleBuild stands in for server/web/console's build (not built for go test): the
// <base href> placeholder plus one hashed asset.
func consoleBuild(marker string) fstest.MapFS {
	return fstest.MapFS{
		"index.html":             {Data: []byte(`<!doctype html><head><base href="/admin/" /><script type="module" src="./assets/index-1a2b3c.js"></script></head>` + marker)},
		"assets/index-1a2b3c.js": {Data: []byte("console.log('console')")},
	}
}

func get(handler http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// requireConsoleAt proves the console is served at mount: index.html (also
// for client routes) carries <base href="mount/">, and config.json and the
// hashed asset resolve beneath it.
func requireConsoleAt(t *testing.T, handler http.Handler, mount, marker string) {
	t.Helper()
	w := get(handler, mount)
	require.Equal(t, http.StatusMovedPermanently, w.Code, mount)
	require.Equal(t, mount+"/", w.Header().Get("Location"))
	for _, page := range []string{mount + "/", mount + "/index.html", mount + "/customers/42"} {
		w = get(handler, page)
		require.Equal(t, http.StatusOK, w.Code, page)
		require.Contains(t, w.Body.String(), `<base href="`+mount+`/">`, page)
		require.Contains(t, w.Body.String(), marker, page)
		require.Equal(t, "no-store", w.Header().Get("Cache-Control"), page)
	}
	w = get(handler, mount+"/config.json")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.True(t, json.Valid(w.Body.Bytes()), w.Body.String())
	w = get(handler, mount+"/assets/index-1a2b3c.js")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "console.log('console')", w.Body.String())
	require.Contains(t, w.Header().Get("Cache-Control"), "immutable")
}

// standaloneConsole serves c from a standalone build on a server whose issuer
// is issuer, a resource server whose persons act at a merchant through a
// token exchanged for it.
func standaloneConsole(issuer string, c *server.AdminConsole) func(*server.Config, *server.Deps) {
	return func(cfg *server.Config, deps *server.Deps) {
		resourced(cfg, deps)
		cfg.Auth.Token.Issuer = issuer
		cfg.AdminConsole = c
		deps.ConsoleAssets = consoleBuild("standalone")
	}
}

// The standalone console switches merchants by token exchange: config.json
// names the exchange (the server's token endpoint, the console's client, the
// resource and the merchant scope), and the token it describes, from the real
// token endpoint, reads the access read of the merchant it names and no other.
// The server's resource defaults to its public API base; without either the
// console does not mount.
func TestConsoleSwitchesMerchantByExchange(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, standaloneConsole("http://127.0.0.1/"+f.schema, &server.AdminConsole{}))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	var boot struct {
		Exchange *struct {
			TokenEndpoint string `json:"token_endpoint"`
			ClientID      string `json:"client_id"`
			Resource      string `json:"resource"`
			Scope         string `json:"scope"`
		} `json:"exchange"`
		Merchant any `json:"merchant"`
	}
	require.NoError(t, json.Unmarshal(get(handler, "/admin/config.json").Body.Bytes(), &boot))
	require.NotNil(t, boot.Exchange)
	require.Equal(t, f.tokenURL(), boot.Exchange.TokenEndpoint)
	require.Equal(t, server.ConsoleClientID, boot.Exchange.ClientID)
	require.Equal(t, resourceID, boot.Exchange.Resource)
	require.Equal(t, billing.ScopeMerchant, boot.Exchange.Scope)
	require.Nil(t, boot.Merchant, "staff choose the merchant")

	ownerID, session := newOwner(t, srv)
	a := provisionOwned(t, srv, uniqueName("console-a"), ownerID)
	b := provisionOwned(t, srv, uniqueName("console-b"), ownerID)
	for _, mid := range []billing.MerchantID{a, b} {
		bound := f.exchanged(t, handler, session, mid)
		require.Equal(t, int64(900), bound.ExpiresIn, "15 minutes, the server's default")
		w := bound.call(t, handler, http.MethodGet, "/v1/admin/access", nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, mid.String(), decode[billing.AdminAccess](t, w).ScopeID)
		require.Equal(t, mid.String(), w.Header().Get("OpenRails-Bound-Merchant"))
	}

	defaulted := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		standaloneConsole("http://127.0.0.1/"+f.schema, &server.AdminConsole{})(cfg, deps)
		cfg.Auth.Resource = authkit.ResourceConfig{}
		cfg.Engine.PublicBillingBaseURL = "https://api.console.e2e.test"
	})
	h, err := standaloneHandler(defaulted)
	require.NoError(t, err)
	require.Contains(t, get(h, "/admin/config.json").Body.String(), `"resource":"https://api.console.e2e.test"`, "the public API base is the resource")
	unbound := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		standaloneConsole("http://127.0.0.1/"+f.schema, nil)(cfg, deps)
		cfg.Auth.Resource = authkit.ResourceConfig{}
	})
	unboundHandler, err := standaloneHandler(unbound)
	require.NoError(t, err)
	require.Contains(t, get(unboundHandler, "/admin/config.json").Body.String(), `"exchange":null`)
}

// The console's bootstrap document names the path AuthKit's JSON API is
// served at: /auth/v1 beneath an origin issuer, else the issuer's path plus
// /v1, the version AuthKit appends.
func TestAdminConsoleFindsAuthKit(t *testing.T) {
	f := newFixture(t)
	for issuer, want := range map[string]string{
		"http://127.0.0.1":             "/auth/v1",
		"http://127.0.0.1/" + f.schema: "/" + f.schema + "/v1",
	} {
		srv := f.newServer(t, standaloneConsole(issuer, &server.AdminConsole{}))
		handler, err := standaloneHandler(srv)
		require.NoError(t, err)
		requireConsoleAt(t, handler, "/admin", "standalone")

		w := get(handler, "/admin/config.json")
		var boot struct {
			AuthBaseURL string `json:"auth_base_url"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&boot))
		require.Equal(t, want, boot.AuthBaseURL, issuer)

		w = get(handler, boot.AuthBaseURL+"/capabilities")
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", issuer, w.Body.String())
	}
}

// The standalone product always owns /admin, including a server whose staff
// use only AuthKit's account and access surfaces.
func TestServerConsoleIsOnByDefault(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, standaloneConsole("http://127.0.0.1", nil))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	requireConsoleAt(t, handler, "/admin", "standalone")
	routes, err := srv.Routes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, route := range routes {
		mux.Handle(route.Method+" "+route.Path, route.Handler)
	}
	requireConsoleAt(t, mux, "/admin", "standalone")
	accountOnly := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		standaloneConsole("http://127.0.0.1", nil)(cfg, deps)
		cfg.RouteGroups = openrails.RouteGroups{Programmatic: true}
	})
	accountHandler, err := standaloneHandler(accountOnly)
	require.NoError(t, err)
	requireConsoleAt(t, accountHandler, "/admin", "standalone")
}

func TestServerConsoleBootsWithoutSignIn(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		standaloneConsole("http://127.0.0.1", nil)(cfg, deps)
		cfg.LocalSignIn = false
	})
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	requireConsoleAt(t, handler, "/admin", "standalone")
	boot := get(handler, "/admin/config.json")
	require.Contains(t, boot.Body.String(), `"local_sign_in":false`)
	require.Contains(t, boot.Body.String(), `"issuers":[]`)
	require.Equal(t, http.StatusUnauthorized, get(handler, "/admin/merchants").Code)
}

// An explicitly supplied build is owned by the server, validated before serving.
func TestStandaloneConsoleRequiresValidAssets(t *testing.T) {
	f := newFixture(t)
	missing := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		standaloneConsole("http://127.0.0.1", nil)(cfg, deps)
		deps.ConsoleAssets = fstest.MapFS{}
	})
	handler, err := standaloneHandler(missing)
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, get(handler, "/admin/").Code)
	for _, tc := range []struct {
		name   string
		assets fstest.MapFS
		want   string
	}{
		{"invalid index", fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}, `<base href="/admin/">`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.buildServer(t, func(cfg *server.Config, deps *server.Deps) {
				standaloneConsole("http://127.0.0.1", &server.AdminConsole{})(cfg, deps)
				deps.ConsoleAssets = tc.assets
			})
			require.ErrorContains(t, err, tc.want)
		})
	}
}
