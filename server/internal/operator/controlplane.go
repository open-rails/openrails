// Package operator holds the standalone server's control-plane operations
// and the declaration package server builds its control plane from. The
// embedded engine has no control plane: package server composes one beside it.
package operator

import (
	"fmt"

	"github.com/open-rails/authkit"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// Join makes a the engine of a standalone server with control plane cp and
// its AuthKit configuration auth: its AuthKit jobs join the engine's River
// fleet, the server's own hosts are kept from merchants' API hosts, and the embedded mount
// refuses. Join before the engine's River is bound.
func Join(a *app.App, cp *controlplane.ControlPlane, auth authkit.Config) error {
	if err := a.Runtime.AddRiverContribution(cp.RiverJobs()); err != nil {
		return fmt.Errorf("control plane: register AuthKit jobs: %w", err)
	}
	a.Runtime.ReserveAPIHosts(auth.Token.Issuer, auth.Resource.PublicURL, auth.Frontend.BaseURL)
	cp.BindMerchants(a.Runtime.Merchants)
	a.Standalone = true
	return nil
}

// Of returns the engine graph and control plane behind a *server.Server, for
// the standalone binary's tooling and tests; package server sets it.
var Of func(srv any) (*app.App, *controlplane.ControlPlane)
