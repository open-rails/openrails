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
	"github.com/open-rails/openrails/internal/standalonedb"
)

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
		cfg.ControlPlane = &openrails.ControlPlaneConfig{Auth: openrails.AuthConfig{
			Issuer: issuer, KeysPath: t.TempDir(), AllowEphemeralSigningKey: true,
			AllowMemory: true, AllowMissingSenders: true, AllowLoopbackHTTP: true, DirectPeerIP: true,
		}}
		// A stand-in console build: web/admin's dist is not built for go test.
		assets := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
		cp, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: assets})
		require.NoError(t, err, issuer)
		t.Cleanup(func() { _ = cp.Close(context.Background()) })
		handler, err := standaloneHandler(cp)
		require.NoError(t, err)

		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/config.json", nil))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var boot struct {
			AuthBaseURL string `json:"auth_base_url"`
		}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&boot))
		require.Equal(t, want, boot.AuthBaseURL, issuer)

		w = httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, boot.AuthBaseURL+"/capabilities", nil))
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", issuer, w.Body.String())
	}
}

// An embedded host mounts the console itself at /admin/, from its own build;
// enabling it without a build refuses to boot.
func TestEmbeddedHostMountsAdminConsole(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: uniqueName("console")}
	cfg.AdminConsole = &openrails.AdminConsoleConfig{Enabled: true, AuthBaseURL: "/api/v1", APIBaseURL: "/billing/v1"}
	_, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: fstest.MapFS{}})
	require.ErrorContains(t, err, "no console build")

	client, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool, ConsoleAssets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>host")}}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	console := client.AdminConsole()
	require.NotNil(t, console)
	w := httptest.NewRecorder()
	console.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/config.json", nil))
	var boot struct {
		AuthBaseURL string `json:"auth_base_url"`
		APIBaseURL  string `json:"api_base_url"`
	}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&boot))
	require.Equal(t, "/api/v1", boot.AuthBaseURL)
	require.Equal(t, "/billing/v1", boot.APIBaseURL)
	w = httptest.NewRecorder()
	console.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/", nil))
	require.Contains(t, w.Body.String(), "host")

	cfg.AdminConsole = nil
	cfg.Merchant.Slug = uniqueName("headless")
	off, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { _ = off.Close(context.Background()) })
	require.Nil(t, off.AdminConsole(), "not enabled, not mounted")
}
