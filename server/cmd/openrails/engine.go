package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/hostdeps"
	"github.com/open-rails/openrails/server/internal/operator"
)

// openServer builds the standalone server over the connections hostdeps
// opens, as run-server does. closeDeps closes them after the server.
func openServer(ctx context.Context) (_ *server.Server, _ *app.App, _ *controlplane.ControlPlane, closeDeps func(), err error) {
	h := hostconfig.FromContext(ctx)
	if h == nil {
		return nil, nil, nil, nil, fmt.Errorf("config not loaded")
	}
	opened, err := hostdeps.Open(ctx, slog.Default(), h)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	srv, err := server.New(ctx, h.Server, server.Deps{Engine: opened.Deps})
	if err != nil {
		opened.Close()
		return nil, nil, nil, nil, fmt.Errorf("bootstrap server: %w", err)
	}
	graph, cp := operator.Of(srv)
	opened.Report(context.WithoutCancel(ctx), graph.Runtime)
	return srv, graph, cp, opened.Close, nil
}
