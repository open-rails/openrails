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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
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
		rt, err := embed.New(t.Context(), embed.Options{
			Config: &config.Config{
				TestMode:          config.CredentialPostureSandbox,
				ProviderWriteMode: config.ProviderWriteModeReadOnly,
				DB:                &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
				ReturnOrigins:     []string{"https://e2e.test"},
				AdminConsole:      &config.AdminConsoleConfig{Enabled: true},
			},
			ConsoleAssets: fstest.MapFS{"index.html": {Data: []byte("<!doctype html>")}},
			PGXPool:       f.pool,
			River:         embed.RiverManagedByOpenRails(f.schema),
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = rt.Close(context.Background()) })
		cp, err := controlplane.Attach(t.Context(), rt, controlplane.Options{Auth: &hostconfig.AuthConfig{
			Issuer: issuer, KeysPath: t.TempDir(), AllowEphemeralSigningKey: true,
			AllowMemory: true, AllowMissingSenders: true, AllowLoopbackHTTP: true, DirectPeerIP: true,
		}})
		require.NoError(t, err, issuer)
		handler, err := cp.Handler()
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
