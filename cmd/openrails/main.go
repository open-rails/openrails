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

	"github.com/open-rails/openrails/cmd/openrails/consoleassets"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

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

			// --provider-write-mode rides the same koanf pipeline as everything
			// else by overwriting PROVIDER_WRITE_MODE before Load: flag beats env
			// beats yaml. The deprecated --mode alias is gone (#710).
			if mode, err := cmd.Flags().GetString("provider-write-mode"); err == nil && strings.TrimSpace(mode) != "" {
				if err := os.Setenv("PROVIDER_WRITE_MODE", strings.TrimSpace(mode)); err != nil {
					return fmt.Errorf("failed to apply --provider-write-mode: %w", err)
				}
			}

			// --test-mode rides the same koanf pipeline as --provider-write-mode:
			// overwrite TEST_MODE before Load so flag beats env beats yaml.
			if posture, err := cmd.Flags().GetString("test-mode"); err == nil && strings.TrimSpace(posture) != "" {
				if err := os.Setenv("TEST_MODE", strings.TrimSpace(posture)); err != nil {
					return fmt.Errorf("failed to apply --test-mode: %w", err)
				}
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			cmd.SetContext(context.WithValue(cmd.Context(), config.ConfigContextKey, cfg))
			return nil
		},
		Long: "Standalone OpenRails server for payments, credits, usage, and subscriptions",
	}

	rootCmd.PersistentFlags().
		StringP("config", "c", "config.yaml", "Path to config file")
	rootCmd.PersistentFlags().
		String("provider-write-mode", "", "Payment-provider write policy: full | limited | readonly (overrides PROVIDER_WRITE_MODE env and config.yaml; required outside development)")
	rootCmd.PersistentFlags().
		String("test-mode", "", "Credential posture: sandbox | live (sandbox uses Stripe test key, NMI sandbox probe, CCBill sandbox, Solana devnet); overrides TEST_MODE env and config.yaml; sandbox is development-only")

	serverCmd := &cobra.Command{
		Use:     "run-server",
		Aliases: []string{"server"},
		RunE:    runServer,
		Short:   "Start the OpenRails server",
	}
	serverCmd.Flags().Bool("no-workers", false, "Disable background workers in this server process")
	serverCmd.Flags().String("merchant-manifest", "", "MODE-1 (#723) merchant manifest converged at boot (default: the conventional "+bootstrap.DefaultMerchantConfigManifestPath+" when present; an explicit path must exist)")

	workerCmd := &cobra.Command{
		Use:   "run-worker",
		RunE:  runWorker,
		Short: "Start OpenRails background workers",
	}

	// migrate is the ONE deliberately RLS-posture-EXEMPT command (or#888): DDL
	// requires the privileged owner role — it creates the merchant_isolation
	// policies and the unprivileged openrails_app role the gate demands, so it
	// cannot run behind that gate. It opens its own handle in internal/migrate;
	// every other command that touches merchant rows goes through openCLIDB.
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
			if err := migrate.Run(ctx, cfg); err != nil {
				return fmt.Errorf("migrations failed: %w", err)
			}
			return nil
		},
	}

	migratePgCmd := &cobra.Command{
		Use:   "pg",
		Short: "Apply all Postgres migrations (River and OpenRails)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			if err := migrate.RunPostgres(ctx, cfg); err != nil {
				return fmt.Errorf("postgres migrations failed: %w", err)
			}
			return nil
		},
	}

	migrateCmd.AddCommand(migrateUpCmd, migratePgCmd)
	// Drop cobra's auto-generated `completion` subcommand.
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, newPushAuthBootstrapCmd(), newPushMerchantConfigCmd(), newDumpMerchantConfigCmd(), newPushCatalogCmd(), newDumpCatalogCmd(), newPullProviderCmd(), newPruneCmd(), newIntentsCmd(), newIntentsLogCmd(), newLedgerAuditCmd())
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

	// ConsoleAssets is nil unless this binary was built with
	// `-tags console_assets` (#754: `task build-console-binary` / Dockerfile).
	embeddedApp, err := embedded.New(embedded.Options{
		Config:        cfg,
		ConsoleAssets: consoleassets.FS(),
		// Standalone keeps self-provisioning (#895): OpenRails builds and runs
		// its own River client in RunWorkers. The declaration is now explicit.
		River: embedded.RiverManagedByOpenRails(),
	})
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
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
	if err := embcp.Attach(context.Background(), embeddedApp.App(), cfg, nil); err != nil {
		cleanupOnError = true
		return fmt.Errorf("attach control plane: %w", err)
	}

	// Startup bootstrap (#327/#531): if the conventional bootstrap manifest is
	// mounted, apply control-plane authority on first run only. Catalog
	// reconciliation stays an explicit CLI/init-job operation.
	if err := applyStartupBootstrap(context.Background(), cfg, embeddedApp.App()); err != nil {
		cleanupOnError = true
		return fmt.Errorf("startup bootstrap: %w", err)
	}

	// MODE 1 (#723/#847): converge the boot merchant manifest EVERY boot —
	// DB rows as projections (insert+overwrite+prune), secrets seeded into the
	// in-memory plane. Same semantics as serverboot.NewServer: the conventional
	// file is optional, an explicit --merchant-manifest path must exist, and
	// merchant_source=api refuses a present manifest.
	manifestPath, err := cmd.Flags().GetString("merchant-manifest")
	if err != nil {
		return fmt.Errorf("failed to read merchant-manifest flag: %w", err)
	}
	if err := serverboot.ReconcileBootMerchantManifest(context.Background(), cfg, embeddedApp.App(), manifestPath, bootNMIProbeV5BaseURL); err != nil {
		cleanupOnError = true
		return err
	}

	cleanupOnError = false

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Public API server (user/admin JWT auth). The full standalone surface is
	// the framework-neutral net/http stack (#670) — the same stack embedded
	// hosts mount.
	publicHandler, err := embedded.StandaloneHandler(embeddedApp)
	if err != nil {
		return fmt.Errorf("build billing http handler: %w", err)
	}
	publicSrv := &http.Server{
		Handler:           publicHandler,
		Addr:              fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
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

	application, err := app.Bootstrap(cfg)
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
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
