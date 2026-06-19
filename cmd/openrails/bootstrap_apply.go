package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

type pushBootstrapOptions struct {
	file      string
	dryRun    bool
	overwrite bool
	prune     bool
}

func newPushBootstrapCmd() *cobra.Command {
	opts := pushBootstrapOptions{file: bootstrap.DefaultBootstrapManifestPath}
	cmd := &cobra.Command{
		Use:     "push-merchant-config",
		Aliases: []string{"push-bootstrap"},
		Short:   "Provision OpenRails merchants (org + issuer-as-owner + secrets + profile) from YAML",
		Args:    validatePushBootstrapArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPushBootstrap(cmd, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.file, "file", "f", bootstrap.DefaultBootstrapManifestPath, "bootstrap manifest YAML file")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "validate and print plans without mutating state")
	cmd.Flags().BoolVar(&opts.overwrite, "overwrite", false, "re-assert manifest secret values over existing (default is seed-once: existing secrets are left untouched)")
	cmd.Flags().BoolVar(&opts.prune, "prune", false, "delete merchant secrets that are absent from the manifest")
	return cmd
}

func validatePushBootstrapArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return err
	}
	path, err := cmd.Flags().GetString("file")
	if err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = bootstrap.DefaultBootstrapManifestPath
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("read bootstrap manifest: %w", err)
	}
	return nil
}

func runPushBootstrap(cmd *cobra.Command, opts pushBootstrapOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	path := strings.TrimSpace(opts.file)
	if path == "" {
		path = bootstrap.DefaultBootstrapManifestPath
	}

	manifest, err := bootstrap.LoadBootstrapManifest(path)
	if err != nil {
		return err
	}

	cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return fmt.Errorf("config not loaded; push-merchant-config requires --config")
	}

	application := &app.App{Config: cfg}
	defer func() {
		if closeErr := application.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("push-merchant-config cleanup failed")
		}
	}()

	if err := embcp.Attach(ctx, application, cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}

	return applyPushBootstrapManifest(ctx, cfg, application, manifest, out, opts.dryRun, bootstrap.MerchantManifestReconcileOptions{
		Overwrite: opts.overwrite,
		Prune:     opts.prune,
	})
}

// startupBootstrapLockKey is the pg_advisory_lock key serializing concurrent
// startup auth/merchant bootstraps (#342). The apply is plan-then-execute with
// no internal transaction, so simultaneous replica cold starts against an empty
// control plane would each plan the same creates and race the inserts. Holding
// this lock across plan+apply makes the second replica plan against the
// converged state instead.
const startupBootstrapLockKey = int64(0x6f72_626f_6f74) // "orboot"

// applyStartupBootstrap applies the bootstrap manifest on the FIRST server start
// only (#527). It is gated by the openrails.bootstrap_state marker: once a boot
// has successfully provisioned, every later boot skips the apply entirely —
// BEFORE the manifest is even loaded — so a stale or malformed manifest can
// never brick a restart of an already-provisioned deployment. Operators change
// merchants after first run with `openrails push-merchant-config`. Catalog state
// is never pushed from startup (`openrails push-merchant-catalog`).
func applyStartupBootstrap(ctx context.Context, cfg *config.Config, a *app.App) error {
	path := resolveBootstrapManifestPath(cfg)
	if path == "" {
		return nil
	}
	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("startup bootstrap: control plane not attached (#469: it is mandatory in standalone mode)")
	}

	conn, err := cp.Pool().Raw().Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire bootstrap lock connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, startupBootstrapLockKey); err != nil {
		return fmt.Errorf("acquire bootstrap advisory lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, startupBootstrapLockKey); err != nil {
			log.WithError(err).Warn("release bootstrap advisory lock")
		}
	}()

	// First-run gate (#527): the schema-qualified marker is a safe identifier
	// (config.validateSchema), so direct interpolation is injection-free.
	schema := cp.Pool().Schema()
	var applied bool
	if err := conn.QueryRow(ctx, fmt.Sprintf(`SELECT EXISTS (SELECT 1 FROM %s.bootstrap_state WHERE singleton)`, schema)).Scan(&applied); err != nil {
		return fmt.Errorf("startup bootstrap: check first-run marker: %w", err)
	}
	if applied {
		log.Info("startup bootstrap: already applied (first-run marker present); skipping — use `openrails push-merchant-config` to change merchants")
		return nil
	}

	manifest, err := bootstrap.LoadBootstrapManifest(path)
	if err != nil {
		return err
	}
	log.WithField("file", path).Info("startup bootstrap: first run — applying bootstrap manifest")
	// Startup is always additive + seed-once: never overwrite or prune on boot.
	if err := applyPushBootstrapManifest(ctx, cfg, a, manifest, log.StandardLogger().Out, false, bootstrap.MerchantManifestReconcileOptions{}); err != nil {
		return err
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf(`INSERT INTO %s.bootstrap_state (singleton) VALUES (true) ON CONFLICT (singleton) DO NOTHING`, schema)); err != nil {
		return fmt.Errorf("startup bootstrap: record first-run marker: %w", err)
	}
	return nil
}

// resolveBootstrapManifestPath returns the conventional bootstrap manifest
// location when that file exists, else "".
func resolveBootstrapManifestPath(_ *config.Config) string {
	if _, err := os.Stat(bootstrap.DefaultBootstrapManifestPath); err == nil {
		return bootstrap.DefaultBootstrapManifestPath
	}
	return ""
}

// applyPushBootstrapManifest provisions the OpenRails merchants declared by the
// manifest: each merchant's backing org + (optional) host-app issuer-as-owner,
// the merchant row, provider secrets, and profile (#527). It intentionally does
// not touch catalog/provider state.
func applyPushBootstrapManifest(ctx context.Context, cfg *config.Config, a *app.App, manifest *bootstrap.BootstrapManifest, out io.Writer, dryRun bool, reconcileOpts bootstrap.MerchantManifestReconcileOptions) error {
	if len(manifest.Merchants) == 0 {
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "merchants: %d declared (dry-run: overwrite=%t prune=%t; no mutations)\n", len(manifest.Merchants), reconcileOpts.Overwrite, reconcileOpts.Prune)
		return nil
	}

	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("merchant bootstrap: control plane not attached")
	}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest.MerchantManifest(), reconcileOpts); err != nil {
		return fmt.Errorf("merchant bootstrap: %w", err)
	}
	fmt.Fprintf(out, "merchants: %d reconciled (overwrite=%t prune=%t)\n", len(manifest.Merchants), reconcileOpts.Overwrite, reconcileOpts.Prune)
	return nil
}
