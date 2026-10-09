package main

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hostconfig"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/server"
)

// serverConfig is the loaded configuration as the standalone server's.
func serverConfig(ctx context.Context) (server.Config, error) {
	h := hostconfig.FromContext(ctx)
	if h.Config == nil {
		return server.Config{}, fmt.Errorf("config not loaded")
	}
	if h.Auth == nil {
		return server.Config{}, fmt.Errorf("auth is required: the standalone server runs its own AuthKit")
	}
	return server.Config{
		Engine: *h.Config, Auth: *h.Auth, ResourceServer: h.ResourceServer, LocalSignIn: h.LocalSignIn,
		CatalogEdits: h.CatalogEdits, AdminConsole: h.AdminConsole, ConsoleIssuer: h.ConsoleIssuer,
		Addr: net.JoinHostPort(h.Host, strconv.Itoa(h.Port)),
	}, nil
}

// openServer builds the standalone server over deps, as run-server does.
func openServer(ctx context.Context, deps openrails.Deps) (*server.Server, *app.App, *controlplane.ControlPlane, error) {
	cfg, err := serverConfig(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	srv, err := server.New(ctx, cfg, server.Deps{Engine: deps})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("bootstrap server: %w", err)
	}
	graph, cp := operator.Of(srv)
	return srv, graph, cp, nil
}

// openEngine builds the engine alone through openrails.New, like any host,
// for commands that need no control plane.
func openEngine(ctx context.Context, cfg *config.Config, deps openrails.Deps) (*openrails.Client, *app.App, error) {
	client, err := openrails.New(ctx, *cfg, deps)
	if err != nil {
		return nil, nil, fmt.Errorf("bootstrap application: %w", err)
	}
	return client, engine.Graph(client), nil
}
