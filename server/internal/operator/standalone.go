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
	RouteGroups   config.RouteGroups
	AdminConsole  *config.ConsoleMount
	ConsoleIssuer *hostconfig.ConsoleIssuer
	// Resource is AuthKit's resource identifier, the console's audience at a
	// trusted issuer.
	Resource string
}

// StandaloneServer builds the standalone surface over the engine graph a and
// its control plane cp: the engine's routes gated by AuthKit's
// Authenticator, AuthKit's own and the admin console.
func StandaloneServer(a *app.App, cp *controlplane.ControlPlane, s Surface) (*server.Server, error) {
	return server.New(server.Dependencies{
		Config:        a.Config,
		Runtime:       a.Runtime,
		ControlPlane:  cp,
		Resource:      s.Resource,
		ConsoleIssuer: s.ConsoleIssuer,
		ConsoleAssets: a.ConsoleAssets,
		RouteGroups:   s.RouteGroups,
		AdminConsole:  s.AdminConsole,
	})
}
