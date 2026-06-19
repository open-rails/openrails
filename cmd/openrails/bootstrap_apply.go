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
	insert    bool
	overwrite bool
	prune     bool
}

type pushMerchantConfigOptions struct {
	file      string
	dryRun    bool
	insert    bool
	overwrite bool
	prune     bool
}

func newPushBootstrapCmd() *cobra.Command {
	opts := pushBootstrapOptions{file: bootstrap.DefaultBootstrapManifestPath}
	cmd := &cobra.Command{
		Use:   "push-bootstrap",
		Short: "Push OpenRails control-plane bootstrap authority from YAML",
		Args:    validatePushBootstrapArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPushBootstrap(cmd, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.file, "file", "f", bootstrap.DefaultBootstrapManifestPath, "bootstrap manifest YAML file")
	addPushMutationFlags(cmd, &opts.dryRun, &opts.insert, &opts.overwrite, &opts.prune,
		"create missing control-plane bootstrap authority",
		"re-assert control-plane bootstrap authority",
		"delete bootstrap-owned extras absent from the manifest (currently no destructive scope)")
	return cmd
}

func newPushMerchantConfigCmd() *cobra.Command {
	opts := pushMerchantConfigOptions{file: bootstrap.DefaultMerchantConfigManifestPath}
	cmd := &cobra.Command{
		Use:   "push-merchant-config",
		Short: "Push OpenRails merchant configuration (org + issuer-as-owner + secrets + profile) from YAML",
		Args:  validatePushMerchantConfigArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPushMerchantConfig(cmd, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.file, "file", "f", bootstrap.DefaultMerchantConfigManifestPath, "merchant config manifest YAML file")
	addPushMutationFlags(cmd, &opts.dryRun, &opts.insert, &opts.overwrite, &opts.prune,
		"create missing merchant/config objects declared by the manifest",
		"re-assert manifest secret/config values over existing state",
		"delete merchant secrets that are absent from the manifest")
	return cmd
}

func addPushMutationFlags(cmd *cobra.Command, dryRun, insert, overwrite, prune *bool, insertHelp, overwriteHelp, pruneHelp string) {
	cmd.Flags().BoolVar(dryRun, "dry-run", false, "deprecated alias for the default plan-only behavior")
	_ = cmd.Flags().MarkHidden("dry-run")
	cmd.Flags().BoolVar(insert, "insert", false, insertHelp)
	cmd.Flags().BoolVar(overwrite, "overwrite", false, overwriteHelp)
	cmd.Flags().BoolVar(prune, "prune", false, pruneHelp)
}

func validateManifestFileArgs(cmd *cobra.Command, args []string, defaultPath, label string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return err
	}
	path, err := cmd.Flags().GetString("file")
	if err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultPath
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("read %s manifest: %w", label, err)
	}
	return nil
}

func validatePushBootstrapArgs(cmd *cobra.Command, args []string) error {
	return validateManifestFileArgs(cmd, args, bootstrap.DefaultBootstrapManifestPath, "bootstrap")
}

func validatePushMerchantConfigArgs(cmd *cobra.Command, args []string) error {
	return validateManifestFileArgs(cmd, args, bootstrap.DefaultMerchantConfigManifestPath, "merchant config")
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
		return fmt.Errorf("config not loaded; push-bootstrap requires --config")
	}

	application := &app.App{Config: cfg}
	defer func() {
		if closeErr := application.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("push-bootstrap cleanup failed")
		}
	}()

	if err := embcp.Attach(ctx, application, cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}

	return applyPushBootstrapManifest(ctx, application, manifest, out, opts.dryRun, bootstrap.MerchantManifestReconcileOptions{
		Insert:    opts.insert,
		Overwrite: opts.overwrite,
		Prune:     opts.prune,
	})
}

func runPushMerchantConfig(cmd *cobra.Command, opts pushMerchantConfigOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	path := strings.TrimSpace(opts.file)
	if path == "" {
		path = bootstrap.DefaultMerchantConfigManifestPath
	}

	manifest, err := bootstrap.LoadMerchantConfigManifest(path)
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

	return applyPushMerchantConfigManifest(ctx, cfg, application, manifest, out, opts.dryRun, bootstrap.MerchantManifestReconcileOptions{
		Insert:    opts.insert,
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

// applyStartupBootstrap applies the bootstrap authority manifest on the FIRST
// server start only (#527/#531). Merchant config and catalog files are never
// reconciled from normal server startup; operators run those explicit CLI
// commands as init jobs or manual operations.
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
		log.Info("startup bootstrap: already applied (first-run marker present); skipping")
		return nil
	}

	manifest, err := bootstrap.LoadBootstrapManifest(path)
	if err != nil {
		return err
	}
	log.WithField("file", path).Info("startup bootstrap: first run — applying bootstrap manifest")
	// Startup is always insert-only + seed-once: never overwrite or prune on boot.
	if err := applyPushBootstrapManifest(ctx, a, manifest, log.StandardLogger().Out, false, bootstrap.MerchantManifestReconcileOptions{Insert: true}); err != nil {
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

// applyPushBootstrapManifest reconciles OpenRails/AuthKit control-plane
// authority. It intentionally does not touch merchant config, merchant secrets,
// catalog/provider state, or remote processors.
func applyPushBootstrapManifest(ctx context.Context, a *app.App, manifest *bootstrap.BootstrapManifest, out io.Writer, dryRun bool, reconcileOpts bootstrap.MerchantManifestReconcileOptions) error {
	if manifest == nil {
		return nil
	}
	if dryRun || !reconcileOpts.HasMutations() {
		fmt.Fprintf(out, "bootstrap authority: org %q declared (plan-only: insert=%t overwrite=%t prune=%t; no mutations)\n", manifest.BootstrapOptions().BootstrapOrgSlug, reconcileOpts.Insert, reconcileOpts.Overwrite, reconcileOpts.Prune)
		return nil
	}
	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("bootstrap: control plane not attached")
	}
	res, err := cp.Bootstrap(ctx, manifest.BootstrapOptions())
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	fmt.Fprintf(out, "bootstrap authority: org %q reconciled (insert=%t overwrite=%t prune=%t service_token_minted=%t)\n", res.BootstrapOrgSlug, reconcileOpts.Insert, reconcileOpts.Overwrite, reconcileOpts.Prune, res.ServiceTokenMinted)
	if res.ServiceTokenMinted && res.ServiceTokenSecret != "" {
		fmt.Fprintf(out, "bootstrap admin service token: %s\n", res.ServiceTokenSecret)
	}
	return nil
}

// applyPushMerchantConfigManifest provisions OpenRails merchants declared by the
// merchant config manifest: backing org + optional host-app issuer-as-owner,
// merchant row, provider secrets, and profile (#527). It intentionally does not
// touch catalog/provider state.
func applyPushMerchantConfigManifest(ctx context.Context, cfg *config.Config, a *app.App, manifest *bootstrap.MerchantManifest, out io.Writer, dryRun bool, reconcileOpts bootstrap.MerchantManifestReconcileOptions) error {
	if manifest == nil || len(manifest.Merchants) == 0 {
		return nil
	}
	if dryRun || !reconcileOpts.HasMutations() {
		fmt.Fprintf(out, "merchants: %d declared (plan-only: insert=%t overwrite=%t prune=%t; no mutations)\n", len(manifest.Merchants), reconcileOpts.Insert, reconcileOpts.Overwrite, reconcileOpts.Prune)
		return nil
	}

	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("merchant config: control plane not attached")
	}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest, reconcileOpts); err != nil {
		return fmt.Errorf("merchant bootstrap: %w", err)
	}
	fmt.Fprintf(out, "merchants: %d reconciled (insert=%t overwrite=%t prune=%t)\n", len(manifest.Merchants), reconcileOpts.Insert, reconcileOpts.Overwrite, reconcileOpts.Prune)
	return nil
}
