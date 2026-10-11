// Package operator holds the standalone server's control-plane operations
// and the declaration package server builds its control plane from. The
// embedded engine has no control plane: package server composes one beside it.
package operator

import (
	"fmt"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/server/internal/controlplane"
)

// Join makes a the engine of a standalone server with control plane cp: its
// AuthKit jobs join the engine's River fleet and the embedded mount refuses.
// Join before the engine's River is bound.
func Join(a *app.App, cp *controlplane.ControlPlane) error {
	if err := a.Runtime.AddRiverContribution(cp.RiverJobs()); err != nil {
		return fmt.Errorf("control plane: register AuthKit jobs: %w", err)
	}
	cp.BindMerchants(a.Runtime.Merchants)
	a.Standalone = true
	return nil
}

// Of returns the engine graph and control plane behind a *server.Server, for
// the standalone binary's tooling and tests; package server sets it.
var Of func(srv any) (*app.App, *controlplane.ControlPlane)
