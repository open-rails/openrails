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
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	embgin "github.com/open-rails/openrails/pkg/embedded/gin"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "billing",
		Short: "OpenRails server",
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
		Long: "Standalone OpenRails server for payments, credits, usage, and subscriptions",
	}

	rootCmd.PersistentFlags().
		StringP("config", "c", "config.yaml", "Path to config file")

	serverCmd := &cobra.Command{
		Use:     "run-server",
		Aliases: []string{"server"},
		RunE:    runServer,
		Short:   "Start the OpenRails server",
	}
	serverCmd.Flags().Bool("no-workers", false, "Disable background workers in this server process")

	workerCmd := &cobra.Command{
		Use:     "worker",
		Aliases: []string{"run-worker"},
		RunE:    runWorker,
		Short:   "Start OpenRails background workers",
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

	auditCmd := &cobra.Command{
		Use:   "audit",
		Short: "Run consistency audit on the OpenRails database",
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
	bootstrapTenantsCmd := &cobra.Command{
		Use:   "bootstrap-tenants",
		Short: "Reconcile the mounted tenant manifest and exit",
		RunE:  bootstrapTenants,
	}
	mintOperatorServiceTokenCmd := &cobra.Command{
		Use:   "mint-operator-service-token",
		Short: "Mint an OpenRails operator service token and print the one-time token",
		RunE:  mintOperatorServiceToken,
	}
	mintOperatorServiceTokenCmd.Flags().String("name", "openrails-operator-manual", "service token display name")
	mintOperatorServiceTokenCmd.Flags().String("org", "", "Bootstrap authority slug for the legacy AuthKit --org bridge (defaults to config/operator)")
	mintOperatorServiceTokenCmd.Flags().String("tenant", "", "OpenRails tenant slug or id (defaults to default)")
	mintOperatorServiceTokenCmd.Flags().StringSlice("permission", nil, "Permission to grant; repeat or comma-separate. Defaults to full operator permissions")

	mintTenantSubjectServiceTokenCmd := &cobra.Command{
		Use:   "mint-tenant-subject-service-token",
		Short: "Mint an OpenRails tenant-subject-scoped service token and print the one-time token",
		RunE:  mintTenantSubjectServiceToken,
	}
	mintTenantSubjectServiceTokenCmd.Flags().String("name", "", "service token display name")
	mintTenantSubjectServiceTokenCmd.Flags().String("org", "", "Bootstrap authority slug for the legacy AuthKit --org bridge that owns the service token (defaults to config/operator)")
	mintTenantSubjectServiceTokenCmd.Flags().String("tenant", "", "OpenRails tenant slug or id (defaults to default)")
	mintTenantSubjectServiceTokenCmd.Flags().String("tenant-subject", "", "OpenRails tenant subject UUID to scope the service token to")
	mintTenantSubjectServiceTokenCmd.Flags().StringSlice("permission", nil, "Permission to grant; repeat or comma-separate. Defaults to openrails:credits:spend")

	mintOperatorJWTCmd := &cobra.Command{
		Use:   "mint-operator-jwt",
		Short: "Mint a JWKS-verifiable bootstrap JWT for /v1/admin/* e2e provisioning",
		RunE:  mintOperatorJWT,
	}
	mintOperatorJWTCmd.Flags().String("org", "", "Bootstrap authority slug for the legacy AuthKit --org bridge (defaults to config/operator)")
	mintOperatorJWTCmd.Flags().String("email", "", "Test user email (default e2e-operator@openrails.test)")
	mintOperatorJWTCmd.Flags().String("username", "", "Test user username (default e2e-operator)")
	mintOperatorJWTCmd.Flags().String("role", "", "Tenant role to assign (default openrails-operator)")

	migrateCmd.AddCommand(migrateUpCmd, migratePgCmd, migrateChCmd)
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, auditCmd, seedDevCatalogCmd, bootstrapTenantsCmd, newBootstrapCmd(), mintOperatorServiceTokenCmd, mintTenantSubjectServiceTokenCmd, mintOperatorJWTCmd, newCatalogCmd())

	if err := rootCmd.Execute(); err != nil {
		log.WithError(err).Fatal("Failed to execute command")
	}
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	noWorkers, err := cmd.Flags().GetBool("no-workers")
	if err != nil {
		return fmt.Errorf("failed to read no-workers flag: %w", err)
	}
	startWorkers := !noWorkers

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

	// Opt into the OpenRails-owned AuthKit control plane (#284): the embedded core
	// no longer builds it, so the standalone attaches it here (no-op in
	// verifier-only mode) before bootstrapping.
	if err := embcp.Attach(context.Background(), embeddedApp, cfg, nil); err != nil {
		cleanupOnError = true
		return fmt.Errorf("attach control plane: %w", err)
	}
	if err := platform.InitTelemetry(); err != nil {
		log.WithError(err).Error("failed to init telemetry")
	}

	// Bootstrap the OpenRails-owned AuthKit control plane (#224) when enabled.
	// Idempotent + a no-op in verifier-only mode. Runs after migrations have been
	// applied (migrations are a separate `billing migrate` step) and at startup.
	if res, err := embcp.RunBootstrap(context.Background(), embeddedApp.App(), controlplane.BootstrapOptions{MintInitialServiceToken: true}); err != nil {
		cleanupOnError = true
		return fmt.Errorf("control plane bootstrap: %w", err)
	} else if res != nil && res.ServiceTokenMinted {
		log.WithFields(log.Fields{
			"service_token_key_id": res.ServiceTokenKeyID,
			"service_token_secret": res.ServiceTokenSecret,
		}).
			Warn("control plane: initial operator service token minted; capture the secret from logs now (shown once)")
	}

	cleanupOnError = false

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Public API server (user/admin JWT auth). The full standalone gin surface
	// lives in the pkg/embedded/gin subpackage now (#285).
	publicHandler, err := embgin.Handler(embeddedApp)
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
	// callers authenticate with OpenRails-issued tenant service tokens against the SAME
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
