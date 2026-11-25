package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/hostdeps"
	"github.com/open-rails/openrails/server/internal/operator"
)

// openServer builds the standalone server over the connections hostdeps
// opens, as run-server does. closeDeps closes them after the server.
// merchantManifest is the boot manifest: an application it or the bootstrap
// manifest declares may read its keys on a private network.
func openServer(ctx context.Context, merchantManifest string) (_ *server.Server, _ *app.App, _ *controlplane.ControlPlane, closeDeps func(), err error) {
	h := hostconfig.FromContext(ctx)
	if h == nil {
		return nil, nil, nil, nil, fmt.Errorf("config not loaded")
	}
	key, err := h.SigningKey.Source()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cfg := h.Server
	private, err := bootstrap.PrivateNetworkApplications(merchantManifest)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if private {
		cfg.AuthKit = privateNetworkKeys
	}
	opened, err := hostdeps.Open(ctx, slog.Default(), h)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	srv, err := server.New(ctx, cfg, server.Deps{Engine: opened.Deps, Auth: authkit.Deps{KeySource: key}})
	if err != nil {
		opened.Close()
		return nil, nil, nil, nil, fmt.Errorf("bootstrap server: %w", err)
	}
	graph, cp := operator.Of(srv)
	opened.Report(context.WithoutCancel(ctx), graph.Runtime)
	return srv, graph, cp, opened.Close, nil
}

// privateNetworkKeys lets AuthKit read the keys of the applications the
// operator declares on a private network.
func privateNetworkKeys(c *authkit.Config) { c.Token.AllowPrivateNetworkJWKS = true }
