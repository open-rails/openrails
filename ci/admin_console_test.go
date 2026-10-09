//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/server"
)

// consoleBuild stands in for web/admin's build (not built for go test): the
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
// is issuer.
func standaloneConsole(issuer string, c *openrails.AdminConsole) func(*server.Config, *server.Deps) {
	return func(cfg *server.Config, deps *server.Deps) {
		cfg.Auth.Issuer = issuer
		cfg.AdminConsole = c
		deps.Engine.ConsoleAssets = consoleBuild("standalone")
	}
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
		srv := f.newServer(t, standaloneConsole(issuer, &openrails.AdminConsole{}))
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

// admin_console.path moves the standalone console (#1127): the server's
// handler and its routes on a host router both serve it there and nothing at
// /admin, and a path over OpenRails' own routes refuses to build the server.
// Without a console selected no console route exists.
func TestStandaloneAdminConsolePath(t *testing.T) {
	f := newFixture(t)
	moved := f.newServer(t, standaloneConsole("http://127.0.0.1", &openrails.AdminConsole{Path: "/billing/admin"}))
	handler, err := standaloneHandler(moved)
	require.NoError(t, err)
	routes, err := moved.Routes()
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Method+" "+r.Path, r.Handler)
	}
	for _, h := range []http.Handler{handler, mux} {
		requireConsoleAt(t, h, "/billing/admin", "standalone")
		require.Equal(t, http.StatusNotFound, get(h, "/admin/").Code)
	}
	off, err := standaloneHandler(f.newServer(t, standaloneConsole("http://127.0.0.1", nil)))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, get(off, "/admin/").Code, "off unless mounted")

	// A hosted product's console extensions read their data from config.json.
	hosted, err := standaloneHandler(f.newServer(t, standaloneConsole("http://127.0.0.1", &openrails.AdminConsole{Extensions: map[string]any{"hosted": map[string]any{"plans": []any{"starter"}}}})))
	require.NoError(t, err)
	require.Contains(t, get(hosted, "/admin/config.json").Body.String(), `"extensions":{"hosted":{"plans":["starter"]}}`)
	_, err = f.buildServer(t, standaloneConsole("http://127.0.0.1", &openrails.AdminConsole{Extensions: map[string]any{"Hosted": true}}))
	require.ErrorContains(t, err, `invalid Routes.AdminConsole.Extensions key "Hosted"`)

	_, err = f.buildServer(t, standaloneConsole("http://127.0.0.1", &openrails.AdminConsole{Path: "/v1"}))
	require.ErrorContains(t, err, `admin_console.path "/v1" overlaps`)
}

// An embedded host mounts the console with the merchant API, from its own
// build, at Routes.AdminConsole.Path on the root router; the console finds the
// API at Routes.Prefix. A missing or broken build, an invalid path, or no
// merchant API fails the mount; without AdminConsole no console route exists.
func TestEmbeddedHostMountsAdminConsole(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: uniqueName("console")}
	staff := openrails.Deps{Postgres: f.pool}
	deny := authtest.Deny{}
	boot := func(assets fstest.MapFS) *openrails.Client {
		deps := staff
		deps.ConsoleAssets = assets
		client, err := openrails.New(t.Context(), cfg, deps)
		require.NoError(t, err)
		t.Cleanup(func() { _ = client.Close(context.Background()) })
		return client
	}
	console := &openrails.AdminConsole{AuthBaseURL: "/api/v1"}
	routes := openrails.Routes{Auth: deny, Prefix: "/billing", Merchant: true, Guards: readsAndWrites, AdminConsole: console}
	_, err := boot(fstest.MapFS{}).Routes(routes)
	require.ErrorContains(t, err, "needs a console build")
	_, err = boot(fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}).Routes(routes)
	require.ErrorContains(t, err, `<base href="/admin/">`)

	client := boot(consoleBuild("host"))
	_, err = client.Routes(openrails.Routes{Auth: deny, Prefix: "/billing", AdminConsole: console})
	require.ErrorContains(t, err, "set Routes.Merchant")
	for _, path := range []string{"/", "admin", "/billing/admin/", "/a/../b", "/a b", `/x"><script>`} {
		_, err = client.Routes(openrails.Routes{Auth: deny, Prefix: "/billing", Merchant: true, Guards: readsAndWrites, AdminConsole: &openrails.AdminConsole{Path: path, AuthBaseURL: "/api/v1"}})
		require.ErrorContains(t, err, "invalid Routes.AdminConsole.Path", path)
	}

	root := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(root, client, routes))
	requireConsoleAt(t, root, "/admin", "host")
	w := get(root, "/admin/config.json")
	var bootstrap struct {
		AuthBaseURL string `json:"auth_base_url"`
		APIBaseURL  string `json:"api_base_url"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&bootstrap))
	require.Equal(t, "/api/v1", bootstrap.AuthBaseURL)
	require.Equal(t, "/billing/v1", bootstrap.APIBaseURL)

	// The host keeps its own /admin pages and mounts the console elsewhere.
	moved := http.NewServeMux()
	moved.HandleFunc("/admin/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("host admin")) })
	require.NoError(t, openrailshttp.Mount(moved, client, openrails.Routes{Auth: deny, Prefix: "/billing", Merchant: true, Guards: readsAndWrites,
		AdminConsole: &openrails.AdminConsole{Path: "/billing/admin", AuthBaseURL: "/api/v1"}}))
	requireConsoleAt(t, moved, "/billing/admin", "host")
	require.Equal(t, "host admin", get(moved, "/admin/").Body.String())

	off := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(off, client, openrails.Routes{Auth: deny, Prefix: "/billing", Merchant: true, Guards: readsAndWrites}))
	require.Equal(t, http.StatusNotFound, get(off, "/admin/").Code, "not selected, not mounted")
}
