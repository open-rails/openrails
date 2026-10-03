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
	"github.com/open-rails/openrails/internal/engine"
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
		cfg.AllowCatalogUpdates = false
		cfg.AdminConsole = &openrails.AdminConsoleConfig{Enabled: true}
		cfg.ControlPlane = &openrails.ControlPlaneConfig{Auth: openrails.AuthConfig{
			Issuer: issuer, KeysPath: t.TempDir(), AllowEphemeralSigningKey: true,
			AllowMemory: true, AllowMissingSenders: true, AllowLoopbackHTTP: true, DirectPeerIP: true,
		}}
		cp, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: f.pool})
		require.NoError(t, err, issuer)
		t.Cleanup(func() { _ = cp.Close(context.Background()) })
		// A stand-in console build: web/admin's dist is not built for go test.
		engine.Graph(cp).ConsoleAssets = fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}}
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
