package operator

import (
	"context"
	"io/fs"
	"net/http"

	"github.com/open-rails/authkit/auth"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/server/internal/controlplane"
	server "github.com/open-rails/openrails/server/internal/http"
)

// Surface is what a standalone server's HTTP surface serves beside the
// engine's routes and the control plane's.
type Surface struct {
	CustomerAdmissionPolicy func(context.Context, auth.Verified, config.CustomerBinding) error
	RouteGroups             config.RouteGroups
	ConsoleDisabled         bool
	ConsoleAssets           fs.FS
	// Resource is AuthKit's resource identifier, the console's audience at a
	// trusted issuer.
	Resource string
	// CheckoutOrigin serves CheckoutHandler, the engine's payment page,
	// there; empty serves none.
	CheckoutOrigin  string
	CheckoutHandler http.Handler
}

// StandaloneServer builds the standalone surface over the engine graph a and
// its control plane cp: the engine's routes gated by AuthKit's
// Authenticator, AuthKit's own and the admin console.
func StandaloneServer(a *app.App, cp *controlplane.ControlPlane, s Surface) (*server.Server, error) {
	return server.New(a.Log, server.Dependencies{
		CustomerAdmissionPolicy: s.CustomerAdmissionPolicy,
		Config:                  a.Config,
		Runtime:                 a.Runtime,
		ControlPlane:            cp,
		Resource:                s.Resource,
		ConsoleAssets:           s.ConsoleAssets,
		RouteGroups:             s.RouteGroups,
		ConsoleDisabled:         s.ConsoleDisabled,
		CheckoutOrigin:          s.CheckoutOrigin,
		CheckoutHandler:         s.CheckoutHandler,
	})
}
