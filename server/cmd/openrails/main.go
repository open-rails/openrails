package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-rails/authkit"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/buildinfo"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/server/internal/bootstrap"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/controlplane"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/open-rails/openrails/server/internal/hostdeps"
)

func main() {
	err := newRootCmd().Execute()
	closeCLI()
	if err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	build := buildinfo.Get()
	rootCmd := &cobra.Command{
		Use:   "openrails",
		Short: "OpenRails server",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// The arguments parsed; a failure from here on is not about usage.
			cmd.SilenceUsage = true
			configPath, err := cmd.Flags().GetString("config")
			if err != nil {
				return fmt.Errorf("failed to get config flag: %w", err)
			}

			if cmd.Flags().Changed("provider-write-mode") {
				return fmt.Errorf("--provider-write-mode was removed: %s", hostconfig.WritePostureHint)
			}
			cfg, err := hostconfig.Load(configPath)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			handler, err := cfg.Logger.Handler(os.Stderr)
			if err != nil {
				return err
			}
			slog.SetDefault(slog.New(handler))
			if cfg.EnvFile != "" {
				slog.Info("config: loaded environment overrides", "path", cfg.EnvFile)
			}

			cmd.SetContext(hostconfig.NewContext(cmd.Context(), cfg))
			return nil
		},
		Long:    "Standalone OpenRails server for payments, credits, usage, and subscriptions",
		Version: build.Version,
	}
	rootCmd.SetVersionTemplate(build.String() + "\n")

	rootCmd.PersistentFlags().
		StringP("config", "c", "config.yaml", "Path to config file")
	// Retired: kept only to refuse it by name.
	rootCmd.PersistentFlags().String("provider-write-mode", "", "")
	_ = rootCmd.PersistentFlags().MarkHidden("provider-write-mode")

	serverCmd := &cobra.Command{
		Use:   "run-server",
		RunE:  runServer,
		Short: "Start the OpenRails server",
	}
	serverCmd.Flags().Bool("no-workers", false, "Disable background workers in this server process (readiness remains not-ready)")
	serverCmd.Flags().String("merchant-manifest", "", "MODE-1 merchant manifest converged at boot (default: the conventional "+bootstrap.DefaultMerchantConfigManifestPath+" when present; an explicit path must exist)")

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
		Short: "Apply OpenRails, AuthKit and River migrations, and the event store's with clickhouse",
		RunE: func(cmd *cobra.Command, args []string) error {
			return applyStandaloneMigrations(cmd.Context(), hostconfig.FromContext(cmd.Context()))
		},
	}

	migratePgCmd := &cobra.Command{
		Use:   "pg",
		Short: "Apply OpenRails, AuthKit and River migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return applyStandaloneMigrations(cmd.Context(), hostconfig.FromContext(cmd.Context()))
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
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, newPushAuthBootstrapCmd(), newPushMerchantConfigCmd(), newDumpMerchantConfigCmd(), newMerchantConfigurationCmd(false), newMerchantConfigurationCmd(true), newDumpCatalogCmd(), newCatalogCmd(), newPruneCmd(), newProposalsCmd(), newUndoRunCmd(), newIntentsCmd(), newIntentsLogCmd(), newLedgerAuditCmd(), newBillingCmd(), newNMICmd(), newSolanaPayCmd(), newBookCmd(), newMerchantCmd(), newMerchantsCmd(), newWorkersCmd(), newProviderRefreshCmd(), newAdminLockoutsCmd(), newDestructiveHoldsCmd(), newUsageCmd())
	rootCmd.AddCommand(versionCmd)
	return rootCmd
}

func runServer(cmd *cobra.Command, args []string) error {
	noWorkers, err := cmd.Flags().GetBool("no-workers")
	if err != nil {
		return fmt.Errorf("failed to read no-workers flag: %w", err)
	}
	manifestPath, err := cmd.Flags().GetString("merchant-manifest")
	if err != nil {
		return fmt.Errorf("failed to read merchant-manifest flag: %w", err)
	}
	slog.Default().Info(buildinfo.Get().String())

	// The boot waits for the database as long as it takes (a failover, a slow
	// start); only a stop signal ends the wait. Meanwhile the process is not
	// listening, which is what "not ready" means to a probe.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv, graph, cp, closeDeps, err := openServer(ctx, manifestPath)
	if err != nil {
		if ctx.Err() != nil {
			slog.Default().InfoContext(ctx, "Shutdown requested while booting; exiting", logx.Err(err))
			return nil
		}
		return err
	}
	defer closeDeps()
	closeOnError := func(err error) error {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return errors.Join(err, srv.Close(closeCtx))
	}

	// A mounted bootstrap manifest applies AuthKit authority on first run
	// only. Catalog reconciliation stays an explicit CLI/init-job operation.
	if err := applyStartupBootstrap(ctx, cp); err != nil {
		return closeOnError(fmt.Errorf("startup bootstrap: %w", err))
	}
	// Provision the manifest's merchants; without Vault the manifest is their
	// configuration. The conventional file is optional; an explicit manifest
	// path must exist.
	if err := serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, cp, manifestPath); err != nil {
		return closeOnError(err)
	}
	// HTTP never serves webhook and async billing APIs when the workers
	// cannot start.
	if noWorkers {
		err = srv.Serve(ctx)
	} else {
		err = srv.Run(ctx)
	}
	if err != nil {
		return err
	}
	slog.Default().InfoContext(ctx, "Billing service shutdown complete")
	return nil
}

func runWorker(cmd *cobra.Command, args []string) error {
	manifestPath, err := cmd.Flags().GetString("merchant-manifest")
	if err != nil {
		return fmt.Errorf("failed to read merchant-manifest flag: %w", err)
	}
	// As in runServer, the database wait ends on a stop signal.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The same server as run-server, without a listener: AuthKit's jobs run
	// in the same fleet.
	srv, graph, cp, closeDeps, err := openServer(ctx, manifestPath)
	if err != nil {
		if ctx.Err() != nil {
			slog.Default().InfoContext(ctx, "Shutdown requested while booting; exiting", logx.Err(err))
			return nil
		}
		return err
	}
	defer closeDeps()
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Close(closeCtx); err != nil {
			slog.Default().ErrorContext(ctx, "Application shutdown encountered issues", logx.Err(err))
		}
	}()
	if err := serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, cp, manifestPath); err != nil {
		return err
	}
	// Background workers only (no HTTP server); fail fast if River cannot start.
	if err := srv.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	slog.Default().InfoContext(ctx, "Shutdown signal received, stopping workers...")
	return nil
}

// applyStandaloneMigrations prepares both schemas without constructing a
// server. Existing single-directory accounts adopt only the explicit platform
// tenant in configuration, never a generated or inferred merchant identity.
func applyStandaloneMigrations(ctx context.Context, host *hostconfig.Config) error {
	if host == nil {
		return errors.New("standalone config is required")
	}
	database, err := openCLIDB(ctx)
	if err != nil {
		return fmt.Errorf("standalone migration pool: %w", err)
	}
	defer database.Close()
	if err := engine.Migrate(ctx, database.Pool(), host.Server.Engine); err != nil {
		return err
	}
	if host.ClickHouse != nil {
		conn, err := hostdeps.ClickHouse(ctx, slog.Default(), host.ClickHouse)
		if err != nil {
			return err
		}
		if conn != nil {
			defer conn.Close()
			chdb := config.ClickHouseDatabase(&host.Server.Engine)
			if err := eventstore.Migrate(ctx, conn, chdb, eventstore.Options{Logger: slog.Default(), Serialize: migrate.ClickHouseLock(database.Pool(), chdb)}); err != nil {
				return err
			}
		}
	}
	// The control plane owns the AuthKit/River schema selection. Preserve its
	// configuration without building its roles, HTTP handler or job workers.
	auth, err := controlplane.AuthKit(&host.Server.Engine, controlplane.Options{KeysPath: host.Server.Auth.Keys.Path})
	if err != nil {
		return err
	}
	return authkit.Migrate(ctx, database.Pool(), auth.Database, authkit.MigrationOptions{Tenancy: auth.Tenancy, Logger: slog.Default()})
}
