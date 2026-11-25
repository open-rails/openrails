// Command standalone is `openrails run-server` with an in-process NMI fake
// (openrailstest/nmimock) behind Deps.ProviderEndpoints, for the standalone
// example's browser tests (examples/standalone/compose.e2e.yaml). The fake
// accepts any credentials and any Collect.js token ending in four digits;
// 0002 declines. A test fixture: no setting of the real server redirects a
// provider.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/openrailstest/nmimock"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/hostdeps"
	"github.com/open-rails/openrails/server/internal/operator"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to config file")
	manifest := flag.String("merchant-manifest", "", "Merchant manifest converged at boot")
	lifetime := flag.Duration("max-lifetime", time.Hour, "Exit after this long")
	flag.Parse()
	if err := run(*configPath, *manifest, *lifetime); err != nil {
		fmt.Fprintln(os.Stderr, "standalone:", err)
		os.Exit(1)
	}
}

func run(configPath, manifest string, lifetime time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, lifetime)
	defer cancel()
	cfg, err := hostconfig.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	handler, err := cfg.Logger.Handler(os.Stderr)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(handler))
	gateway := nmimock.New(nmimock.Options{})
	defer gateway.Close()
	opened, err := hostdeps.Open(ctx, slog.Default(), cfg)
	if err != nil {
		return err
	}
	defer opened.Close()
	opened.Deps.ProviderEndpoints = openrails.ProviderEndpoints{NMIGatewayURL: gateway.URL()}
	srv, err := server.New(ctx, cfg.Server, server.Deps{Engine: opened.Deps})
	if err != nil {
		return fmt.Errorf("bootstrap server: %w", err)
	}
	graph, cp := operator.Of(srv)
	opened.Report(context.WithoutCancel(ctx), graph.Runtime)
	if err := serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, cp, manifest); err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return errors.Join(err, srv.Close(closeCtx))
	}
	return srv.Run(ctx)
}
