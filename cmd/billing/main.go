package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/audit"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/embedded"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "billing",
		Short: "Open Rails Billing Service",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			configPath, err := cmd.Flags().GetString("config")
			if err != nil {
				return fmt.Errorf("failed to get config flag: %w", err)
			}

			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			cmd.SetContext(context.WithValue(cmd.Context(), config.ConfigContextKey, cfg))
			return nil
		},
		Long: "Standalone Open Rails billing service for handling payments and subscriptions",
	}

	rootCmd.PersistentFlags().
		StringP("config", "c", "config.yaml", "Path to config file")

	serverCmd := &cobra.Command{
		Use:     "run-server",
		Aliases: []string{"server"},
		RunE:    runServer,
		Short:   "Start the billing service server",
	}
	serverCmd.Flags().Bool("start-workers", false, "Start background workers alongside the HTTP server")

	workerCmd := &cobra.Command{
		Use:   "worker",
		RunE:  runWorker,
		Short: "Start the billing service background workers",
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
		Short: "Apply all Postgres migrations (River → Billing)",
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
		Short:   "Apply all ClickHouse migrations (Billing analytics)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			if err := migrate.RunClickHouse(ctx, cfg); err != nil {
				return fmt.Errorf("clickhouse migrations failed: %w", err)
			}
			return nil
		},
	}

	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Run consistency audit on the billing database",
		RunE:  runAudit,
	}
	auditCmd.Flags().String("format", "table", "Output format: table, json, csv")
	auditCmd.Flags().String("user-id", "", "Filter to specific user ID")
	auditCmd.Flags().String("severity", "", "Filter by minimum severity: CRITICAL, HIGH, MEDIUM, LOW")
	auditCmd.Flags().StringSlice("category", nil, "Filter by category (can be repeated)")

	seedDevCatalogCmd := &cobra.Command{
		Use:   "seed-dev-catalog",
		Short: "Seed a minimal dev billing catalog for local migrations",
		RunE:  seedDevCatalog,
	}

	migrateCmd.AddCommand(migrateUpCmd, migratePgCmd, migrateChCmd)
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, auditCmd, seedDevCatalogCmd)

	if err := rootCmd.Execute(); err != nil {
		log.WithError(err).Fatal("Failed to execute command")
	}
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	startWorkers, err := cmd.Flags().GetBool("start-workers")
	if err != nil {
		return fmt.Errorf("failed to read start-workers flag: %w", err)
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

	cleanupOnError = false

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Public API server (user/admin JWT auth)
	publicSrv := &http.Server{
		Handler: embeddedApp.Handler(),
		Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
	}

	// Private/Service API server (X-API-KEY auth for server-to-server calls)
	var privateSrv *http.Server
	if cfg.PrivatePort > 0 {
		privateSrv = &http.Server{
			Handler: embeddedApp.ServiceHandler(),
			Addr:    fmt.Sprintf("%s:%d", cfg.Host, cfg.PrivatePort),
		}
	}

	// Start public server in a goroutine
	go func() {
		log.Infof("Starting public billing server on %s", publicSrv.Addr)
		if err := publicSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.WithError(err).Fatal("Failed to start public server")
		}
	}()

	// Start private/service server in a goroutine (if configured)
	if privateSrv != nil {
		go func() {
			log.Infof("Starting private/service billing server on %s", privateSrv.Addr)
			if err := privateSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.WithError(err).Fatal("Failed to start private server")
			}
		}()
	}

	var (
		workerDone chan struct{}
		workerErr  atomic.Pointer[error]
	)
	if startWorkers {
		workerCtx := cmd.Context()
		workerDone = make(chan struct{})
		go func() {
			defer close(workerDone)
			log.Info("Starting billing background workers")
			err := embeddedApp.RunWorkers(workerCtx)
			errCopy := err
			workerErr.Store(&errCopy)

			switch {
			case err == nil:
				log.Warn("Background workers exited without error; continuing without workers")
			case err == context.Canceled:
				// Normal shutdown path.
			default:
				log.WithError(err).Error("Background workers stopped unexpectedly; continuing without workers")
			}
		}()
	}

	// Wait for interrupt signal to gracefully shutdown the server.
	// If workers stop unexpectedly, keep serving HTTP (especially useful in dev/zero-config mode).
	<-sigChan
	log.Info("Shutdown signal received, shutting down server...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := publicSrv.Shutdown(shutdownCtx); err != nil {
		log.WithError(err).Error("Public server forced to shutdown")
	}
	if privateSrv != nil {
		if err := privateSrv.Shutdown(shutdownCtx); err != nil {
			log.WithError(err).Error("Private server forced to shutdown")
		}
	}

	if err := embeddedApp.Close(shutdownCtx); err != nil {
		log.WithError(err).Error("Application shutdown encountered issues")
	}

	// Note: we intentionally do NOT cancel the worker context during shutdown.
	// `embeddedApp.Close()` stops workers via River's Stop() and avoids generating
	// noisy "context canceled" errors during normal shutdown.

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

	log.Info("Billing service shutdown complete")
	return nil
}

func runWorker(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
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

func runAudit(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	application, err := app.Bootstrap(cfg)
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() {
		if err := application.Close(context.Background()); err != nil {
			log.WithError(err).Error("Application cleanup failed")
		}
	}()

	// Parse flags
	format, _ := cmd.Flags().GetString("format")
	userID, _ := cmd.Flags().GetString("user-id")
	severityStr, _ := cmd.Flags().GetString("severity")
	categories, _ := cmd.Flags().GetStringSlice("category")

	opts := audit.Options{
		UserID:     userID,
		Format:     format,
		Categories: categories,
	}

	if severityStr != "" {
		opts.Severity = audit.Severity(severityStr)
	}

	// Create checker and run audit
	checker := audit.NewChecker(application.Runtime.DB.GetDB())
	findings, summary, err := checker.Run(cmd.Context(), opts)
	if err != nil {
		return fmt.Errorf("audit failed: %w", err)
	}

	// Format and output results
	formatter := audit.GetFormatter(format)
	if err := formatter.Format(os.Stdout, findings, summary); err != nil {
		return fmt.Errorf("format output: %w", err)
	}

	// Return non-zero exit if critical issues found
	if summary.BySeverity[audit.SeverityCritical] > 0 {
		os.Exit(1)
	}

	return nil
}
