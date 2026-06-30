package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	embgin "github.com/open-rails/openrails/pkg/embedded/gin"
)

func main() {
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
			// beats yaml. --mode is retained as a deprecated alias.
			if mode, err := cmd.Flags().GetString("provider-write-mode"); err == nil && strings.TrimSpace(mode) != "" {
				if err := os.Setenv("PROVIDER_WRITE_MODE", strings.TrimSpace(mode)); err != nil {
					return fmt.Errorf("failed to apply --provider-write-mode: %w", err)
				}
			} else if mode, err := cmd.Flags().GetString("mode"); err == nil && strings.TrimSpace(mode) != "" {
				if err := os.Setenv("MODE", strings.TrimSpace(mode)); err != nil {
					return fmt.Errorf("failed to apply --mode: %w", err)
				}
			}

			// --test-mode mirrors --mode: overwrite the TEST_MODE env var before
			// Load so flag beats env beats yaml. Only applied when the flag was
			// passed explicitly, so an unset flag never clobbers TEST_MODE.
			if cmd.Flags().Changed("test-mode") {
				testMode, err := cmd.Flags().GetBool("test-mode")
				if err != nil {
					return fmt.Errorf("failed to get test-mode flag: %w", err)
				}
				if err := os.Setenv("TEST_MODE", strconv.FormatBool(testMode)); err != nil {
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
		String("mode", "", "Deprecated alias for --provider-write-mode")
	rootCmd.PersistentFlags().
		Bool("test-mode", false, "Use sandbox rail credentials (Stripe test key, NMI sandbox probe, CCBill sandbox, Solana devnet); overrides TEST_MODE env and config.yaml; development only")

	serverCmd := &cobra.Command{
		Use:     "run-server",
		Aliases: []string{"server"},
		RunE:    runServer,
		Short:   "Start the OpenRails server",
	}
	serverCmd.Flags().Bool("no-workers", false, "Disable background workers in this server process")

	workerCmd := &cobra.Command{
		Use:   "run-worker",
		RunE:  runWorker,
		Short: "Start OpenRails background workers",
	}

	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Manage all database tables (Postgres and ClickHouse)",
	}

	migrateUpCmd := &cobra.Command{
		Use:   "up",
		Short: "Apply all database migrations (Postgres and ClickHouse independently)",
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

	migrateChCmd := &cobra.Command{
		Use:     "ch",
		Aliases: []string{"clickhouse"},
		Short:   "Apply all ClickHouse migrations (OpenRails analytics)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			if err := migrate.RunClickHouse(ctx, cfg); err != nil {
				return fmt.Errorf("clickhouse migrations failed: %w", err)
			}
			return nil
		},
	}

	migrateCmd.AddCommand(migrateUpCmd, migratePgCmd, migrateChCmd)
	// Drop cobra's auto-generated `completion` subcommand.
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, newPushAuthBootstrapCmd(), newPushMerchantConfigCmd(), newDumpMerchantConfigCmd(), newPushCatalogCmd(), newDumpCatalogCmd(), newPullProviderCmd(), newIntentsCmd(), newIntentsLogCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	noWorkers, err := cmd.Flags().GetBool("no-workers")
	if err != nil {
		return fmt.Errorf("failed to read no-workers flag: %w", err)
	}
	startWorkers := !noWorkers
	config.LogStartupStatus(cfg)
	if err := config.ConfigureProcessGlobals(cfg); err != nil {
		return err
	}

	if cfg.Env == "production" || cfg.Env == "prod" {
		gin.SetMode(gin.ReleaseMode)
	}

	embeddedApp, err := embedded.New(embedded.Options{Config: cfg})
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
	// mounted, apply control-plane authority on first run only. Merchant config
	// and catalog reconciliation stay explicit CLI/init-job operations.
	if err := applyStartupBootstrap(context.Background(), cfg, embeddedApp.App()); err != nil {
		cleanupOnError = true
		return fmt.Errorf("startup bootstrap: %w", err)
	}

	cleanupOnError = false

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Public API server (user/admin JWT auth). The full standalone gin surface
	// lives in the pkg/embedded/gin subpackage now (#285).
	publicHandler, err := embgin.StandaloneHandler(embeddedApp)
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
	if err := config.ConfigureProcessGlobals(cfg); err != nil {
		return err
	}

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
