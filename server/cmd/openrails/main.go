package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/buildinfo"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/server/internal/bootstrap"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// bootNMIProbeV5BaseURL is a test-only override for the test_mode NMI arm
// probe target during the boot-manifest reconcile; empty in production.
var bootNMIProbeV5BaseURL string

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

			// Flags are a koanf confmap overlay above env: flag beats env
			// beats yaml. They never write the process env.
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
		Short: "Apply OpenRails and River migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			ctx := cmd.Context()
			return applyStandaloneMigrations(ctx, cfg)
		},
	}

	migratePgCmd := &cobra.Command{
		Use:   "pg",
		Short: "Apply OpenRails and River migrations",
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
	rootCmd.AddCommand(serverCmd, workerCmd, migrateCmd, newPushAuthBootstrapCmd(), newPushMerchantConfigCmd(), newDumpMerchantConfigCmd(), newMerchantConfigurationCmd(false), newMerchantConfigurationCmd(true), newApplyCatalogCmd(), newDumpCatalogCmd(), newCatalogCmd(), newPullProviderCmd(), newPruneCmd(), newConvergeCmd(), newUndoRunCmd(), newIntentsCmd(), newIntentsLogCmd(), newLedgerAuditCmd(), newBillingCmd(), newSandboxCmd(), newSolanaSignerCmd(), newNMICmd(), newSolanaPayCmd(), newAccessCutoverCmd(), newBookCmd(), newMerchantPostureCmd(), newMerchantsCmd(), newWorkersCmd(), newAdminLockoutsCmd())
	rootCmd.AddCommand(versionCmd)
	return rootCmd
}

func runServer(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	noWorkers, err := cmd.Flags().GetBool("no-workers")
	if err != nil {
		return fmt.Errorf("failed to read no-workers flag: %w", err)
	}
	manifestPath, err := cmd.Flags().GetString("merchant-manifest")
	if err != nil {
		return fmt.Errorf("failed to read merchant-manifest flag: %w", err)
	}
	log.Info(buildinfo.Get().String())
	config.LogStartupStatus(cfg)

	// The boot waits for the database as long as it takes (a failover, a slow
	// start); only a stop signal ends the wait. Meanwhile the process is not
	// listening, which is what "not ready" means to a probe.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv, graph, cp, err := openServer(ctx, openrails.Deps{})
	if err != nil {
		if ctx.Err() != nil {
			log.WithError(err).Info("Shutdown requested while booting; exiting")
			return nil
		}
		return err
	}
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
	listener := hostconfig.FromContext(cmd.Context())
	if err := serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, cp, manifestPath, listener.MerchantManifestOverlays, bootNMIProbeV5BaseURL); err != nil {
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
	log.Info("Billing service shutdown complete")
	return nil
}

func runWorker(cmd *cobra.Command, args []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	config.LogStartupStatus(cfg)
	manifestPath, err := cmd.Flags().GetString("merchant-manifest")
	if err != nil {
		return fmt.Errorf("failed to read merchant-manifest flag: %w", err)
	}
	// As in runServer, the database wait ends on a stop signal.
	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The same server as run-server, without a listener: AuthKit's jobs run
	// in the same fleet.
	srv, graph, cp, err := openServer(ctx, openrails.Deps{})
	if err != nil {
		if ctx.Err() != nil {
			log.WithError(err).Info("Shutdown requested while booting; exiting")
			return nil
		}
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Close(closeCtx); err != nil {
			log.WithError(err).Error("Application shutdown encountered issues")
		}
	}()
	if err := serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, cp, manifestPath, hostconfig.FromContext(cmd.Context()).MerchantManifestOverlays, bootNMIProbeV5BaseURL); err != nil {
		return err
	}
	// Background workers only (no HTTP server); fail fast if River cannot start.
	if err := srv.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	log.Info("Shutdown signal received, stopping workers...")
	return nil
}

// applyStandaloneMigrations does what server.New does first: billing and
// River. AuthKit migrates itself when the server builds it.
func applyStandaloneMigrations(ctx context.Context, cfg *config.Config) error {
	pool, err := pgxpool.New(ctx, config.DBConnectionString(cfg.DB))
	if err != nil {
		return fmt.Errorf("standalone migration pool: %w", err)
	}
	defer pool.Close()
	return engine.Migrate(ctx, pool, *cfg)
}
