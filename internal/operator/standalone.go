package operator

import (
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	server "github.com/open-rails/openrails/internal/http"
)

// Surface is what a standalone server's HTTP surface serves beside the
// engine's routes and the control plane's.
type Surface struct {
	CatalogEdits   bool
	AdminConsole   *config.AdminConsole
	ConsoleIssuer  *config.ConsoleIssuer
	ResourceServer *config.ResourceServerConfig
	// Issuer is the control plane's AuthKit issuer.
	Issuer string
}

// StandaloneServer builds the standalone surface over the engine graph a and
// its control plane cp: the engine's routes gated by the control plane's Auth,
// the control plane's own routes, AuthKit's and the admin console.
func StandaloneServer(a *app.App, cp *controlplane.ControlPlane, s Surface) (*server.Server, error) {
	return server.New(server.Dependencies{
		Config:         a.Config,
		Runtime:        a.Runtime,
		Redis:          a.RedisClient,
		Authenticator:  cp.UserAuthenticator(),
		ControlPlane:   cp,
		Issuer:         s.Issuer,
		ResourceServer: s.ResourceServer,
		ConsoleIssuer:  s.ConsoleIssuer,
		ConsoleAssets:  a.ConsoleAssets,
		CatalogEdits:   s.CatalogEdits,
		AdminConsole:   s.AdminConsole,
	})
}
