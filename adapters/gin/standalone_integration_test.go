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
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func TestStandaloneCustomerExposureUnderConsole(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureSandbox, SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}, DB: &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)}, AdminConsole: &config.AdminConsoleConfig{Enabled: true}}
	reject := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return nil, billingauth.ErrUnauthenticated
	})
	runtime, err := embed.New(ctx, embed.Options{Config: cfg, River: embed.RiverManagedByOpenRails(), ConsoleAssets: fstest.MapFS{"index.html": {Data: []byte("console page")}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(ctx)) })
	cp, err := controlplane.Attach(ctx, runtime, controlplane.Options{Auth: &hostconfig.AuthConfig{Issuer: "https://gin.openrails.test", KeysPath: t.TempDir(), AllowMemory: true, AllowEphemeralSigningKey: true, AllowMissingSenders: true, AllowPrivateNetworkJWKS: true, DirectPeerIP: true}}, embed.CustomerRoutesConfig{Prefix: "/admin/custom", DelegatedAuthenticator: reject})
	require.NoError(t, err)
	bundle, err := Routes(cp)
	require.ErrorContains(t, err, "conflicting native subtree")
	require.Nil(t, bundle, "reject the configured bundle before mutating the host router")
}
