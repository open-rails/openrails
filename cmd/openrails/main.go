package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/cmd/openrails/consoleassets"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/standalonedb"
)

type standaloneAuthKey struct{}

func standaloneAuth(ctx context.Context) *hostconfig.AuthConfig {
	auth, _ := ctx.Value(standaloneAuthKey{}).(*hostconfig.AuthConfig)
	return auth
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

			ctx := context.WithValue(cmd.Context(), config.ConfigContextKey, cfg.Config)
			cmd.SetContext(context.WithValue(ctx, standaloneAuthKey{}, cfg.Auth))
			return nil
		},
		Long: "Standalone OpenRails server for payments, credits, usage, and subscriptions",
	}

	rootCmd.PersistentFlags().
		StringP("config", "c", "config.yaml", "Path to config file")
	rootCmd.PersistentFlags().
		String("provider-write-mode", "", "Payment-provider write policy: full | limited | readonly (overrides PROVIDER_WRITE_MODE env and config.yaml; omission defaults to readonly)")
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

	// migrate is the ONE deliberately RLS-posture-EXEMPT command (or#888): DDL
	// requires the privileged owner role — it creates the merchant_isolation
	// policies and provisions direct access for the host runtime login, so it
	// cannot run behind that gate. It opens its own handle in internal/migrate;
	// every other command that touches merchant rows goes through openCLIDB.
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage all database tables",
	}

	var runtimeDatabaseURL string
	migrateCmd.PersistentFlags().StringVar(&runtimeDatabaseURL, "runtime-database-url", "", "Host runtime database connection to provision with library access")

	migrateUpCmd := &cobra.Command{
		Use:   "up",
		Short: "Apply all database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			return applyStandaloneMigrations(ctx, cfg, runtimeDatabaseURL)
		},
	}

	migratePgCmd := &cobra.Command{
		Use:   "pg",
		Short: "Apply standalone Postgres migrations (OpenRails, River, and AuthKit)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			return applyStandaloneMigrations(ctx, cfg, runtimeDatabaseURL)
		},
	}

	migrateCmd.AddCommand(migrateUpCmd, migratePgCmd, newMigrateStatusCmd())
	// Drop cobra's auto-generated `completion` subcommand.
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, newPushAuthBootstrapCmd(), newPushMerchantConfigCmd(), newDumpMerchantConfigCmd(), newMerchantConfigurationCmd(false), newMerchantConfigurationCmd(true), newApplyCatalogCmd(), newDumpCatalogCmd(), newPullProviderCmd(), newPruneCmd(), newConvergeCmd(), newUndoRunCmd(), newIntentsCmd(), newIntentsLogCmd(), newLedgerAuditCmd(), newBillingCmd(), newSandboxCmd(), newSolanaSignerCmd())
	return rootCmd
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	noWorkers, err := cmd.Flags().GetBool("no-workers")
	if err != nil {
		return fmt.Errorf("failed to read no-workers flag: %w", err)
	}
	startWorkers := !noWorkers
	config.LogStartupStatus(cfg)

	// xs-007 row 40: the boot waits for the database for as long as it takes
	// — a failover, a slow start — and only an operator's stop signal ends the
	// wait. While waiting the process is not listening, which is exactly what
	// "not ready" means to whoever is probing it; the 60 s budget this
	// replaced turned a two-minute failover into a crash loop.
	bootCtx, stopBoot := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopBoot()

	// ConsoleAssets is nil unless this binary was built with
	// `-tags console_assets` (#754: `task build-console-binary` / Dockerfile).
	embeddedApp, err := embed.New(bootCtx, embed.Options{
		Config:        cfg,
		ConsoleAssets: consoleassets.FS(),
		// Standalone keeps self-provisioning (#895): OpenRails builds and runs
		// its own River client in RunWorkers. The declaration is now explicit.
		River: embed.RiverManagedByOpenRails(),
	})
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
			if err := embeddedApp.Close(context.Background()); err != nil {
				log.WithError(err).Error("Application cleanup failed")
			}
		}
	}()

	// Attach the OpenRails-owned AuthKit control plane (#284). MANDATORY in
	// standalone mode (#469): construction failure exits non-zero — there is no
	// verifier-only downgrade.
	cp, err := controlplane.Attach(context.Background(), embeddedApp, controlplane.Options{Auth: standaloneAuth(cmd.Context())})
	if err != nil {
		cleanupOnError = true
		return fmt.Errorf("attach control plane: %w", err)
	}
	graph := app.HostGraph(embeddedApp)

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
	if err := serverboot.ReconcileBootMerchantManifest(context.Background(), cfg, graph, manifestPath, bootNMIProbeV5BaseURL); err != nil {
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
	publicHandler, err := cp.Handler()
	if err != nil {
		return fmt.Errorf("build billing http handler: %w", err)
	}
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
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
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

	var (
		workerDone   chan struct{}
		workerCancel context.CancelFunc
		workerErr    atomic.Pointer[error]
	)
	if startWorkers {
		workerCtx, cancel := context.WithCancel(cmd.Context())
		workerCancel = cancel
		workerDone = make(chan struct{})
		go func() {
			defer close(workerDone)
			log.Info("Starting billing background workers")
			err := embeddedApp.RunWorkers(workerCtx)
			errCopy := err
			workerErr.Store(&errCopy)

			switch {
			case err == nil:
				log.Warn("Background workers exited without error; shutting down HTTP servers")
			case err == context.Canceled:
				// Normal shutdown path.
			default:
				log.WithError(err).Error("Background workers stopped unexpectedly; shutting down HTTP servers")
			}
		}()
	}

	// Wait for interrupt signal or worker termination. HTTP must not continue
	// serving webhook/async billing APIs after workers fail to start or stop
	// unexpectedly.
	workerStopped := false
	if workerDone != nil {
		select {
		case <-sigChan:
			log.Info("Shutdown signal received, shutting down server...")
		case <-workerDone:
			workerStopped = true
			log.Error("Background workers stopped; shutting down server...")
		}
	} else {
		<-sigChan
		log.Info("Shutdown signal received, shutting down server...")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if workerCancel != nil {
		workerCancel()
	}
	if err := publicSrv.Shutdown(shutdownCtx); err != nil {
		log.WithError(err).Error("Public server forced to shutdown")
	}

	if err := embeddedApp.Close(shutdownCtx); err != nil {
		log.WithError(err).Error("Application shutdown encountered issues")
	}

	if workerDone != nil {
		select {
		case <-workerDone:
		case <-shutdownCtx.Done():
			log.Warn("Timed out waiting for background workers to stop")
		}
	}

	if p := workerErr.Load(); p != nil && *p != nil && *p != context.Canceled {
		return *p
	}
	if workerStopped {
		return fmt.Errorf("background workers stopped")
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
	application, err := serverboot.NewWorker(bootCtx, cfg, &serverboot.Options{Auth: standaloneAuth(cmd.Context()), MerchantManifestPath: manifestPath, NMIProbeV5BaseURL: bootNMIProbeV5BaseURL})
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
			if err := application.Close(context.Background()); err != nil {
				log.WithError(err).Error("Application cleanup failed")
			}
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	cleanupOnError = false

	// Start only background workers (no HTTP server). Fail fast if River cannot start.
	workerCtx, cancel := context.WithCancel(cmd.Context())
	workerDone := make(chan struct{})
	var workerErr atomic.Pointer[error]
	go func() {
		defer close(workerDone)
		err := application.Runtime.RunWorkers(workerCtx)
		errCopy := err
		workerErr.Store(&errCopy)
	}()

	select {
	case <-workerDone:
		if p := workerErr.Load(); p != nil && *p != nil && *p != context.Canceled {
			cancel()
			if err := application.Close(context.Background()); err != nil {
				log.WithError(err).Error("Application cleanup failed")
			}
			return *p
		}
		log.Warn("Background workers exited without error; waiting for shutdown signal")
		<-sigChan
		log.Info("Shutdown signal received, stopping workers...")
		cancel()
	case <-sigChan:
		log.Info("Shutdown signal received, stopping workers...")
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := application.Close(shutdownCtx); err != nil {
		log.WithError(err).Error("Application shutdown encountered issues")
	}

	<-workerDone
	if p := workerErr.Load(); p != nil && *p != nil && *p != context.Canceled {
		return *p
	}

	log.Info("Billing service workers shutdown complete")
	return nil
}

// The CLI is the standalone composition root: billing and AuthKit initialize
// independently through their owning libraries, using migration credentials.
func applyStandaloneMigrations(ctx context.Context, cfg *config.Config, runtimeURL string) error {
	pool, err := pgxpool.New(ctx, cfg.DB.GetConnectionString())
	if err != nil {
		return fmt.Errorf("standalone identity migration pool: %w", err)
	}
	defer pool.Close()
	var runtime *pgxpool.Pool
	if runtimeURL != "" {
		runtime, err = pgxpool.New(ctx, runtimeURL)
		if err != nil {
			return fmt.Errorf("standalone runtime pool: %w", err)
		}
		defer runtime.Close()
	}
	if err := migrate.ApplyPostgresMigrations(ctx, pool, migrate.Options{Schema: cfg.DB.SchemaName(), RuntimePool: runtime}); err != nil {
		return err
	}
	return standalonedb.ApplyAuthKit(ctx, pool, runtime)
}
