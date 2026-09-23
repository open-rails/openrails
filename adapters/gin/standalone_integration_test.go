//go:build integration

package openrailsgin

import (
	"context"
	"net/http"
	"testing"
	"testing/fstest"

	"github.com/gin-gonic/gin"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestStandaloneCustomerExposureUnderConsole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}, Auth: &config.AuthConfig{Issuer: "https://gin.openrails.test", KeysPath: t.TempDir()}, AdminConsole: &config.AdminConsoleConfig{Enabled: true}}
	reject := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	})
	runtime, err := embed.New(ctx, embed.Options{HTTP: &embed.HTTPConfig{Standalone: true, CustomerRoutes: []embed.CustomerRoutesConfig{{Prefix: "/admin/custom", DelegatedAuthenticator: reject}}}, Config: cfg, River: embed.RiverManagedByOpenRails(), ConsoleAssets: fstest.MapFS{"index.html": {Data: []byte("console page")}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(ctx)) })
	_, err = controlplane.Attach(ctx, runtime, controlplane.Options{})
	require.NoError(t, err)
	bundle, err := Routes(runtime)
	require.ErrorContains(t, err, "conflicting native subtree")
	require.Nil(t, bundle, "reject the configured bundle before mutating the host router")
}
