package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/doujins-org/doujins-billing/config"
	"github.com/doujins-org/doujins-billing/internal/app"
	"github.com/doujins-org/doujins-billing/internal/audit"
	"github.com/doujins-org/doujins-billing/internal/migrate"
	"github.com/doujins-org/doujins-billing/pkg/embedded"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "billing",
		Short: "Doujins Billing Service",
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
		Long: "Standalone billing service for handling payments and subscriptions",
	}

	rootCmd.PersistentFlags().
		StringP("config", "c", "config.yaml", "Path to config file")

	serverCmd := &cobra.Command{
		Use:     "run-server",
		Aliases: []string{"server"},
		RunE:    runServer,
		Short:   "Start the billing service server",
	}

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
		Short: "Apply all Postgres migrations (AuthKit → River → Billing)",
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

	migrateCmd.AddCommand(migrateUpCmd, migratePgCmd, migrateChCmd)
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, auditCmd)

	if err := rootCmd.Execute(); err != nil {
		log.WithError(err).Fatal("Failed to execute command")
	}
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)

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
			Handler: embeddedApp.PrivateHandler(),
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

	// Wait for interrupt signal to gracefully shutdown the server
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
	errCh := make(chan error, 1)
	go func() {
		errCh <- application.Runtime.RunWorkers(workerCtx)
	}()

	select {
	case err := <-errCh:
		cancel()
		if err := application.Close(context.Background()); err != nil {
			log.WithError(err).Error("Application cleanup failed")
		}
		return err
	case <-sigChan:
		log.Info("Shutdown signal received, stopping workers...")
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := application.Close(shutdownCtx); err != nil {
		log.WithError(err).Error("Application shutdown encountered issues")
	}

	workerErr := <-errCh
	if workerErr != nil && workerErr != context.Canceled {
		return workerErr
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
