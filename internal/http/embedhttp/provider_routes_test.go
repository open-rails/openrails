package embedhttp

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/stretchr/testify/require"
)

func TestProviderRoutesCredentialAuthority(t *testing.T) {
	for _, source := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		for _, backendWritable := range []bool{false, true} {
			for _, explicit := range []bool{false, true} {
				rt := &app.Runtime{
					Config:            &config.Config{SecretBackend: source, AllowCatalogUpdates: true},
					RouteCapabilities: &routesurface.RuntimeCapabilities{SecretWrite: backendWritable},
				}
				var override *routesurface.ProviderRoutes
				if explicit {
					all := routesurface.AllProviderRoutes()
					override = &all
				}
				routes := ProviderRoutesForRuntime(rt, override)
				wantWrite := source == config.SecretBackendDB && backendWritable
				require.Equal(t, wantWrite, routes.SecretWrite, "source=%s backend=%v explicit=%v", source, backendWritable, explicit)
				require.False(t, routes.SolanaSigning, "an explicit route override cannot manufacture a runtime signer")
				require.True(t, routes.Webhooks)
				require.Equal(t, backendWritable, rt.RouteCapabilities.SecretWrite, "provider visibility must not disable managed alert URL storage")
			}
		}
	}
}
