package operator

import (
	"context"
	"github.com/open-rails/authkit/auth"
	"io/fs"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	server "github.com/open-rails/openrails/server/internal/http"
)

// Surface is what a standalone server's HTTP surface serves beside the
// engine's routes and the control plane's.
type Surface struct {
	CustomerAdmissionPolicy func(context.Context, auth.Verified, config.CustomerBinding) error
	RouteGroups             config.RouteGroups
	AdminConsole            *hostconfig.ConsoleConfig
	ConsoleAssets           fs.FS
	// Resource is AuthKit's resource identifier, the console's audience at a
	// trusted issuer.
	Resource string
	// CheckoutOrigin serves the payment page there, from PaymentPageAssets;
	// empty serves none.
	CheckoutOrigin    string
	PaymentPageAssets fs.FS
}

// StandaloneServer builds the standalone surface over the engine graph a and
// its control plane cp: the engine's routes gated by AuthKit's
// Authenticator, AuthKit's own and the admin console.
func StandaloneServer(a *app.App, cp *controlplane.ControlPlane, s Surface) (*server.Server, error) {
	return server.New(server.Dependencies{
		CustomerAdmissionPolicy: s.CustomerAdmissionPolicy,
		Config:                  a.Config,
		Runtime:                 a.Runtime,
		ControlPlane:            cp,
		Resource:                s.Resource,
		ConsoleAssets:           s.ConsoleAssets,
		RouteGroups:             s.RouteGroups,
		AdminConsole:            s.AdminConsole,
		CheckoutOrigin:          s.CheckoutOrigin,
		PaymentPageAssets:       s.PaymentPageAssets,
	})
}
