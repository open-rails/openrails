package main

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/hostdeps"
	"github.com/open-rails/openrails/server/internal/operator"
)

// serverConfig is the loaded configuration as the standalone server's.
func serverConfig(ctx context.Context) (server.Config, error) {
	h := hostconfig.FromContext(ctx)
	if h.Config == nil {
		return server.Config{}, fmt.Errorf("config not loaded")
	}
	cfg := server.Config{
		Engine: *h.Config, Auth: h.Auth, LocalSignIn: h.LocalSignIn, Naming: h.Naming,
		RouteGroups: h.RouteGroups, AdminConsole: h.AdminConsole,
		Addr:       net.JoinHostPort(h.Host, strconv.Itoa(h.Port)),
		DrainDelay: h.DrainDelay, ShutdownTimeout: h.ShutdownTimeout,
	}
	// AuthKit takes CF-Connecting-IP from Cloudflare's egress alone.
	if len(h.Proxies.Cloudflare) > 0 {
		http := authkit.HTTPConfig{}
		if cfg.Auth.HTTP != nil {
			http = *cfg.Auth.HTTP
		}
		if !http.DirectPeerIP && len(http.TrustedProxies) == 0 && len(http.CloudflareProxies) == 0 {
			http.TrustedProxies, http.CloudflareProxies = h.Proxies.Trusted, h.Proxies.Cloudflare
		}
		cfg.Auth.HTTP = &http
	}
	if h.PrivatePort != 0 {
		cfg.PrivateAddr = net.JoinHostPort(h.Host, strconv.Itoa(h.PrivatePort))
	}
	return cfg, nil
}

// openServer builds the standalone server over the connections hostdeps
// opens, as run-server does. closeDeps closes them after the server.
func openServer(ctx context.Context) (_ *server.Server, _ *app.App, _ *controlplane.ControlPlane, closeDeps func(), err error) {
	cfg, err := serverConfig(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	h := hostconfig.FromContext(ctx)
	key, err := h.SigningKey.Source()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	opened, err := hostdeps.Open(ctx, h)
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
