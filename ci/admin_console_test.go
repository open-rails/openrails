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
	"github.com/open-rails/openrails/internal/standalonedb"
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

// The console's bootstrap document names the path AuthKit's JSON API is
// served at: /auth/v1 beneath an origin issuer, else the issuer's path plus
// /v1, the version AuthKit appends.
func TestAdminConsoleFindsAuthKit(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool))
	for issuer, want := range map[string]string{
		"http://127.0.0.1":             "/auth/v1",
		"http://127.0.0.1/" + f.schema: "/" + f.schema + "/v1",
	} {
		cfg := f.config()
		cfg.AdminConsole = &openrails.AdminConsoleConfig{Enabled: true}
		cfg.ControlPlane = controlPlane(t, issuer)
		cp, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: consoleBuild("standalone")})
		require.NoError(t, err, issuer)
		t.Cleanup(func() { _ = cp.Close(context.Background()) })
		handler, err := standaloneHandler(cp)
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

func controlPlane(t *testing.T, issuer string) *openrails.ControlPlaneConfig {
	return &openrails.ControlPlaneConfig{Auth: openrails.AuthConfig{
		Issuer: issuer, KeysPath: t.TempDir(), AllowEphemeralSigningKey: true,
		AllowMemory: true, AllowMissingSenders: true, AllowLoopbackHTTP: true, DirectPeerIP: true,
	}}
}

// admin_console.path moves the standalone console (#1127): the binary's
// handler and a library mount both serve it there and nothing at /admin, and
// a path over OpenRails' own routes refuses to build the surface.
func TestStandaloneAdminConsolePath(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, standalonedb.ApplyAuthKit(t.Context(), f.pool))
	cfg := f.config()
	cfg.AdminConsole = &openrails.AdminConsoleConfig{Enabled: true, Path: "/billing/admin"}
	cfg.ControlPlane = controlPlane(t, "http://127.0.0.1")
	cp, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: consoleBuild("standalone")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = cp.Close(context.Background()) })

	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, cp))
	for _, h := range []http.Handler{handler, mux} {
		requireConsoleAt(t, h, "/billing/admin", "standalone")
		require.Equal(t, http.StatusNotFound, get(h, "/admin/").Code)
	}

	cfg.AdminConsole.Path = "/v1"
	overlapping, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: consoleBuild("standalone")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = overlapping.Close(context.Background()) })
	_, err = standaloneHandler(overlapping)
	require.ErrorContains(t, err, `admin_console.path "/v1" overlaps`)
	_, err = overlapping.Routes()
	require.ErrorContains(t, err, `admin_console.path "/v1" overlaps`)
}

// An embedded host mounts the console itself at Config.AdminConsole.Path, from
// its own build; enabling it without a build, with a build lacking the
// <base href> placeholder, or with an invalid path refuses to boot. Mounted
// elsewhere, it refuses requests instead of serving a page whose assets 404.
func TestEmbeddedHostMountsAdminConsole(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: uniqueName("console")}
	cfg.AdminConsole = &openrails.AdminConsoleConfig{Enabled: true, AuthBaseURL: "/api/v1", APIBaseURL: "/billing/v1"}
	_, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: fstest.MapFS{}})
	require.ErrorContains(t, err, "no console build")
	_, err = openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}})
	require.ErrorContains(t, err, `<base href="/admin/">`)
	for _, path := range []string{"/", "admin", "/billing/admin/", "/a/../b", "/a b", `/x"><script>`} {
		cfg.AdminConsole.Path = path
		_, err = openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: consoleBuild("host")})
		require.ErrorContains(t, err, "invalid admin_console.path", path)
	}

	cfg.AdminConsole.Path = ""
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: consoleBuild("host")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	console := client.AdminConsole()
	require.NotNil(t, console)
	root := http.NewServeMux()
	root.Handle("/admin/", console)
	root.Handle("/admin", console)
	requireConsoleAt(t, root, "/admin", "host")
	w := get(root, "/admin/config.json")
	var boot struct {
		AuthBaseURL string `json:"auth_base_url"`
		APIBaseURL  string `json:"api_base_url"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&boot))
	require.Equal(t, "/api/v1", boot.AuthBaseURL)
	require.Equal(t, "/billing/v1", boot.APIBaseURL)

	// The host keeps its own /admin pages and mounts the console beneath /billing.
	cfg.AdminConsole.Path = "/billing/admin"
	cfg.Merchant.Slug = uniqueName("moved")
	moved, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: consoleBuild("host")})
	require.NoError(t, err)
	t.Cleanup(func() { _ = moved.Close(context.Background()) })
	root = http.NewServeMux()
	root.HandleFunc("/admin/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("host admin")) })
	root.Handle("/billing/admin/", moved.AdminConsole())
	root.Handle("/billing/admin", moved.AdminConsole())
	requireConsoleAt(t, root, "/billing/admin", "host")
	require.Equal(t, "host admin", get(root, "/admin/").Body.String())

	misplaced := http.NewServeMux()
	misplaced.Handle("/admin/", moved.AdminConsole())
	misplaced.Handle("/billing/admin/", http.StripPrefix("/billing/admin", moved.AdminConsole()))
	for _, path := range []string{"/admin/", "/billing/admin/customers"} {
		w = get(misplaced, path)
		require.Equal(t, http.StatusInternalServerError, w.Code, path)
		require.Contains(t, w.Body.String(), "mounted outside its configured path", path)
	}

	cfg.AdminConsole = nil
	cfg.Merchant.Slug = uniqueName("headless")
	off, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = off.Close(context.Background()) })
	require.Nil(t, off.AdminConsole(), "not enabled, not mounted")
}
