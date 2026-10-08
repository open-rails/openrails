package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/buildinfo"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/hostconfig"
	"github.com/open-rails/openrails/internal/operator"
)

// standaloneAuth is the control plane's identity configuration, nil for
// database-only commands.
func standaloneAuth(ctx context.Context) *config.AuthConfig {
	cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil || cfg.ControlPlane == nil {
		return nil
	}
	return &cfg.ControlPlane.Auth
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// bootNMIProbeV5BaseURL is a test-only override for the #348 test_mode NMI
// arm probe target during the boot-manifest reconcile; empty in production.
var bootNMIProbeV5BaseURL string

func newRootCmd() *cobra.Command {
	build := buildinfo.Get()
	rootCmd := &cobra.Command{
		Use:   "openrails",
		Short: "OpenRails server",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			configPath, err := cmd.Flags().GetString("config")
			if err != nil {
				return fmt.Errorf("failed to get config flag: %w", err)
			}

			// Flags ride the same koanf pipeline as everything else as a
			// confmap overlay above env: flag beats env beats yaml (or#915 —
			// the old path wrote PROVIDER_WRITE_MODE/TEST_MODE into the
			// process env before Load, a back-door the env doctrine bans).
			// The deprecated --mode alias is gone (#710).
			var loadOpts []hostconfig.LoadOption
			if mode, err := cmd.Flags().GetString("provider-write-mode"); err == nil && strings.TrimSpace(mode) != "" {
				loadOpts = append(loadOpts, hostconfig.WithOverride("provider_write_mode", strings.TrimSpace(mode)))
			}
			if posture, err := cmd.Flags().GetString("test-mode"); err == nil && strings.TrimSpace(posture) != "" {
				loadOpts = append(loadOpts, hostconfig.WithOverride("test_mode", strings.TrimSpace(posture)))
			}

			load := hostconfig.Load
			if isDatabaseOnlyBillingCommand(cmd) {
				load = hostconfig.LoadDatabase
			}
			cfg, err := load(configPath, loadOpts...)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			// The standalone server always runs the control plane (#469).
			if cfg.Auth != nil {
				cfg.Config.ControlPlane = &config.ControlPlaneConfig{Auth: *cfg.Auth, ResourceServer: cfg.ResourceServer}
			}
			cmd.SetContext(hostconfig.NewContext(context.WithValue(cmd.Context(), config.ConfigContextKey, cfg.Config), cfg))
			return nil
		},
		Long:    "Standalone OpenRails server for payments, credits, usage, and subscriptions",
		Version: build.Version,
	}
	rootCmd.SetVersionTemplate(build.String() + "\n")

	rootCmd.PersistentFlags().
		StringP("config", "c", "config.yaml", "Path to config file")
	rootCmd.PersistentFlags().
		String("provider-write-mode", "", "Payment-provider write policy: full | limited | readonly (overrides PROVIDER_WRITE_MODE env and config.yaml; required to boot)")
	rootCmd.PersistentFlags().
		String("test-mode", "", "Credential posture: sandbox | live (sandbox uses Stripe test key, NMI sandbox probe, CCBill sandbox, Solana devnet); overrides TEST_MODE env and config.yaml; posture must be explicit")

	serverCmd := &cobra.Command{
		Use:   "run-server",
		RunE:  runServer,
		Short: "Start the OpenRails server",
	}
	serverCmd.Flags().Bool("no-workers", false, "Disable background workers in this server process (readiness remains not-ready)")
	serverCmd.Flags().String("merchant-manifest", "", "MODE-1 (#723) merchant manifest converged at boot (default: the conventional "+bootstrap.DefaultMerchantConfigManifestPath+" when present; an explicit path must exist)")

	workerCmd := &cobra.Command{
		Use:   "run-worker",
		RunE:  runWorker,
		Short: "Start OpenRails background workers",
	}
	workerCmd.Flags().String("merchant-manifest", "", "Merchant manifest converged before starting workers (default: the conventional "+bootstrap.DefaultMerchantConfigManifestPath+" when present; an explicit path must exist)")

	// migrate opens its own handle; every other command that touches merchant
	// rows goes through openCLIDB.
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage all database tables",
	}

	migrateUpCmd := &cobra.Command{
		Use:   "up",
		Short: "Apply all database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			return applyStandaloneMigrations(ctx, cfg)
		},
	}

	migratePgCmd := &cobra.Command{
		Use:   "pg",
		Short: "Apply standalone Postgres migrations (OpenRails, River, and AuthKit)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			return applyStandaloneMigrations(ctx, cfg)
		},
	}

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version, commit and build date",
		Args:  cobra.NoArgs,
		// Needs no config file.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintln(cmd.OutOrStdout(), build)
		},
	}

	migrateCmd.AddCommand(migrateUpCmd, migratePgCmd, newMigrateStatusCmd())
	// Drop cobra's auto-generated `completion` subcommand.
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, newPushAuthBootstrapCmd(), newPushMerchantConfigCmd(), newDumpMerchantConfigCmd(), newMerchantConfigurationCmd(false), newMerchantConfigurationCmd(true), newApplyCatalogCmd(), newDumpCatalogCmd(), newCatalogCmd(), newPullProviderCmd(), newPruneCmd(), newConvergeCmd(), newUndoRunCmd(), newIntentsCmd(), newIntentsLogCmd(), newLedgerAuditCmd(), newBillingCmd(), newSandboxCmd(), newSolanaSignerCmd(), newNMICmd(), newSolanaPayCmd())
	rootCmd.AddCommand(versionCmd)
	return rootCmd
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	noWorkers, err := cmd.Flags().GetBool("no-workers")
	if err != nil {
		return fmt.Errorf("failed to read no-workers flag: %w", err)
	}
	startWorkers := !noWorkers
	log.Info(buildinfo.Get().String())
	config.LogStartupStatus(cfg)

	// xs-007 row 40: the boot waits for the database for as long as it takes
	// — a failover, a slow start — and only an operator's stop signal ends the
	// wait. While waiting the process is not listening, which is exactly what
	// "not ready" means to whoever is probing it; the 60 s budget this
	// replaced turned a two-minute failover into a crash loop.
	bootCtx, stopBoot := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopBoot()

	// The standalone binary builds its engine like any host, with the
	// OpenRails-owned control plane (mandatory here, #469) and OpenRails-managed
	// River (#895). The admin console is web/admin's build, when present.
	client, err := openrails.New(bootCtx, *cfg, openrails.Deps{})
	if err != nil {
		if bootCtx.Err() != nil {
			log.WithError(err).Info("Shutdown requested while booting; exiting")
			return nil
		}
		return fmt.Errorf("bootstrap application: %w", err)
	}
	stopBoot()
	cleanupOnError := true
	defer func() {
		if cleanupOnError {
			if err := client.Close(context.Background()); err != nil {
				log.WithError(err).Error("Application cleanup failed")
			}
		}
	}()
	graph := engine.Graph(client)

	// Startup bootstrap (#327/#531): if the conventional bootstrap manifest is
	// mounted, apply control-plane authority on first run only. Catalog
	// reconciliation stays an explicit CLI/init-job operation.
	if err := applyStartupBootstrap(context.Background(), cfg, graph); err != nil {
		cleanupOnError = true
		return fmt.Errorf("startup bootstrap: %w", err)
	}

	// Reload snapshot credentials and seed absent merchant metadata. Existing
	// metadata is preserved unless an explicit application changes it. The
	// conventional file is optional; an explicit manifest path must exist.
	manifestPath, err := cmd.Flags().GetString("merchant-manifest")
	if err != nil {
		return fmt.Errorf("failed to read merchant-manifest flag: %w", err)
	}
	listener := hostconfig.FromContext(cmd.Context())
	if err := serverboot.ReconcileBootMerchantManifest(context.Background(), graph.Config, graph, manifestPath, listener.MerchantManifestOverlays, bootNMIProbeV5BaseURL); err != nil {
		cleanupOnError = true
		return err
	}
	// Bind request-side producers before HTTP can accept work, including when
	// --no-workers delegates execution to a separate process. This starts no workers.
	if err := graph.Runtime.InitRiver(cmd.Context()); err != nil {
		return fmt.Errorf("bind standalone job producers: %w", err)
	}

	cleanupOnError = false

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Public API server (user/admin JWT auth). The full standalone surface is
	// the framework-neutral net/http stack (#670) — the same stack embedded
	// hosts mount.
	publicServer, err := operator.StandaloneServer(graph, listener.Routes())
	if err != nil {
		return fmt.Errorf("build billing http handler: %w", err)
	}
	publicHandler := publicServer.Handler()
	// xs-007 row 37: no request-wide WriteTimeout. It was set before the
	// handler knew its work, and at 30 s it sat below a route's own 50 s
	// budget: a payment-method replacement committed at the provider and the
	// client got EOF. A route that has a budget declares it
	// (httprequest.Request.Budget) and owns its deadline; every provider and
	// database call underneath carries its own I/O bound. What stays is what
	// observes the PEER, not the work: ReadHeaderTimeout and ReadTimeout bound
	// a client that opened a connection and is not sending its request
	// (slowloris — bytes not arriving is the observation), IdleTimeout bounds
	// a keep-alive connection between requests.
	publicSrv := &http.Server{
		Handler:           publicHandler,
		Addr:              fmt.Sprintf("%s:%d", listener.Host, listener.Port),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	// Issue #222: there is no separate private/service listener. Server-to-server
	// callers authenticate with OpenRails-issued merchant API keys against the SAME
	// public API surface (publicSrv); embedded hosts use the in-process facade.

	// Start public server in a goroutine
	go func() {
		log.Infof("Starting public billing server on %s", publicSrv.Addr)
		if err := publicSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Fatal("Failed to start public server")
		}
	}()

	// HTTP must not serve webhook and async billing APIs when the workers
	// cannot start.
	var workerErr error
	if startWorkers {
		log.Info("Starting billing background workers")
		if workerErr = client.Start(cmd.Context()); workerErr != nil {
			log.WithError(workerErr).Error("Background workers failed to start; shutting down server...")
		}
	}
	if workerErr == nil {
		<-sigChan
		log.Info("Shutdown signal received, shutting down server...")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := publicSrv.Shutdown(shutdownCtx); err != nil {
		log.WithError(err).Error("Public server forced to shutdown")
	}

	if err := client.Close(shutdownCtx); err != nil {
		log.WithError(err).Error("Application shutdown encountered issues")
	}

	if workerErr != nil {
		return workerErr
	}
	log.Info("Billing service shutdown complete")
	return nil
}

func runWorker(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	config.LogStartupStatus(cfg)

	// xs-007 row 40: see runServer — the database wait ends on a stop signal.
	bootCtx, stopBoot := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stopBoot()
	manifestPath, err := cmd.Flags().GetString("merchant-manifest")
	if err != nil {
		return fmt.Errorf("failed to read merchant-manifest flag: %w", err)
	}
	// The same engine as run-server, without a listener: the control plane
	// contributes its AuthKit jobs to the fleet.
	client, err := openrails.New(bootCtx, *cfg, openrails.Deps{})
	if err != nil {
		if bootCtx.Err() != nil {
			log.WithError(err).Info("Shutdown requested while booting; exiting")
			return nil
		}
		return fmt.Errorf("bootstrap application: %w", err)
	}
	stopBoot()
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := client.Close(shutdownCtx); err != nil {
			log.WithError(err).Error("Application shutdown encountered issues")
		}
	}()
	graph := engine.Graph(client)
	if err := serverboot.ReconcileBootMerchantManifest(cmd.Context(), graph.Config, graph, manifestPath, hostconfig.FromContext(cmd.Context()).MerchantManifestOverlays, bootNMIProbeV5BaseURL); err != nil {
		return err
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	// Background workers only (no HTTP server); fail fast if River cannot start.
	if err := client.Start(cmd.Context()); err != nil {
		return err
	}
	<-sigChan
	log.Info("Shutdown signal received, stopping workers...")
	log.Info("Billing service workers shutdown complete")
	return nil
}

// applyStandaloneMigrations migrates like any host (openrails.Migrate): billing,
// River and, with the control plane, AuthKit.
func applyStandaloneMigrations(ctx context.Context, cfg *config.Config) error {
	pool, err := pgxpool.New(ctx, config.DBConnectionString(cfg.DB))
	if err != nil {
		return fmt.Errorf("standalone migration pool: %w", err)
	}
	defer pool.Close()
	return openrails.Migrate(ctx, pool, *cfg)
}
