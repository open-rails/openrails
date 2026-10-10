package operator

import (
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	server "github.com/open-rails/openrails/server/internal/http"
)

// Surface is what a standalone server's HTTP surface serves beside the
// engine's routes and the control plane's.
type Surface struct {
	RouteGroups    config.RouteGroups
	AdminConsole   *config.ConsoleMount
	ConsoleIssuer  *hostconfig.ConsoleIssuer
	ResourceServer *hostconfig.ResourceServerConfig
	// Issuer is the control plane's AuthKit issuer.
	Issuer string
}

// StandaloneServer builds the standalone surface over the engine graph a and
// its control plane cp: the engine's routes gated by the control plane's Auth,
// AuthKit's and the admin console.
func StandaloneServer(a *app.App, cp *controlplane.ControlPlane, s Surface) (*server.Server, error) {
	return server.New(server.Dependencies{
		Config:         a.Config,
		Runtime:        a.Runtime,
		Authenticator:  cp.UserAuthenticator(),
		ControlPlane:   cp,
		Issuer:         s.Issuer,
		ResourceServer: s.ResourceServer,
		ConsoleIssuer:  s.ConsoleIssuer,
		ConsoleAssets:  a.ConsoleAssets,
		RouteGroups:    s.RouteGroups,
		AdminConsole:   s.AdminConsole,
	})
}
