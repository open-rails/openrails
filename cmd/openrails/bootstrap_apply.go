package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	authkit "github.com/open-rails/authkit"
	akembedded "github.com/open-rails/authkit/embedded"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

type pushAuthBootstrapOptions struct {
	file        string
	dryRun      bool
	startupOnly bool
	name        string
}

type pushMerchantConfigOptions struct {
	file      string
	dryRun    bool
	insert    bool
	overwrite bool
	prune     bool
	seed      bool
}

func newPushAuthBootstrapCmd() *cobra.Command {
	opts := pushAuthBootstrapOptions{file: bootstrap.DefaultBootstrapManifestPath}
	cmd := &cobra.Command{
		Use:   "push-auth-bootstrap",
		Short: "Push standalone AuthKit authority from AuthKit bootstrap YAML",
		Args:  validatePushAuthBootstrapArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPushAuthBootstrap(cmd, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.file, "file", "f", bootstrap.DefaultBootstrapManifestPath, "AuthKit bootstrap manifest YAML file")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "plan AuthKit authority bootstrap without mutating state")
	cmd.Flags().BoolVar(&opts.startupOnly, "startup-only", false, "apply at most once using AuthKit's bootstrap marker")
	cmd.Flags().StringVar(&opts.name, "name", "openrails", "AuthKit startup marker name when --startup-only is set")
	return cmd
}

func newPushMerchantConfigCmd() *cobra.Command {
	opts := pushMerchantConfigOptions{file: bootstrap.DefaultMerchantConfigManifestPath}
	cmd := &cobra.Command{
		Use:   "push-merchant-config",
		Short: "Push OpenRails merchant configuration (merchant group + issuer-as-owner + secrets + profile) from YAML",
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
	cmd.Flags().BoolVar(&opts.seed, "seed", false, "merchant_source=api only (#851): seed-once import — create missing merchants/PSPs/secrets into the persistent stores; existing values are never touched and the HTTP APIs own merchant config afterward")
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

func validatePushAuthBootstrapArgs(cmd *cobra.Command, args []string) error {
	return validateManifestFileArgs(cmd, args, bootstrap.DefaultBootstrapManifestPath, "AuthKit authority")
}

func validatePushMerchantConfigArgs(cmd *cobra.Command, args []string) error {
	return validateManifestFileArgs(cmd, args, bootstrap.DefaultMerchantConfigManifestPath, "merchant config")
}

func runPushAuthBootstrap(cmd *cobra.Command, opts pushAuthBootstrapOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	path := strings.TrimSpace(opts.file)
	if path == "" {
		path = bootstrap.DefaultBootstrapManifestPath
	}

	manifest, err := akembedded.LoadBootstrapManifestFile(path)
	if err != nil {
		return err
	}

	cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return fmt.Errorf("config not loaded; push-auth-bootstrap requires --config")
	}

	application := &app.App{Config: cfg}
	defer func() {
		if closeErr := application.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("push-auth-bootstrap cleanup failed")
		}
	}()

	if err := embcp.Attach(ctx, application, cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}

	return applyAuthKitAuthorityManifest(ctx, application, manifest, out, authkit.BootstrapReconcileOptions{
		DryRun:      opts.dryRun,
		StartupOnly: opts.startupOnly,
		Name:        opts.name,
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
	// #723/#851: in api mode (MODE 2) the APIs own merchant config, so a bare
	// run refuses (two truths) — but --seed runs the command as a seed-once
	// importer into the persistent stores. Manifest mode applies DB projections
	// (secrets are validated but never persisted; the server holds them in memory).
	reconcileOpts, err := bootstrap.ResolvePushMerchantConfigOptions(cfg, opts.seed, opts.insert, opts.overwrite, opts.prune)
	if err != nil {
		return err
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

	return applyPushMerchantConfigManifest(ctx, cfg, application, manifest, out, opts.dryRun, reconcileOpts)
}

type dumpMerchantConfigOptions struct {
	slug           string
	out            string
	includeSecrets bool
}

func newDumpMerchantConfigCmd() *cobra.Command {
	opts := dumpMerchantConfigOptions{}
	cmd := &cobra.Command{
		Use:   "dump-merchant-config",
		Short: "Dump a merchant's OpenRails configuration as push-merchant-config YAML; secrets are redacted unless --include-secrets is set",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDumpMerchantConfig(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.slug, "slug", "", "merchant slug to dump (required)")
	cmd.Flags().StringVarP(&opts.out, "out", "o", "", "write YAML to this file (default: stdout)")
	cmd.Flags().BoolVar(&opts.includeSecrets, "include-secrets", false, "include plaintext merchant secret values in the dumped YAML")
	_ = cmd.MarkFlagRequired("slug")
	return cmd
}

func runDumpMerchantConfig(cmd *cobra.Command, opts dumpMerchantConfigOptions) error {
	ctx := cmd.Context()
	cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return fmt.Errorf("config not loaded; dump-merchant-config requires --config")
	}

	application := &app.App{Config: cfg}
	defer func() {
		if closeErr := application.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("dump-merchant-config cleanup failed")
		}
	}()
	if err := embcp.Attach(ctx, application, cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}

	manifest, err := bootstrap.DumpMerchantConfig(ctx, cfg, embcp.Get(application), opts.slug, bootstrap.DumpMerchantConfigOptions{
		IncludeSecrets: opts.includeSecrets,
	})
	if err != nil {
		return err
	}
	encoded, err := bootstrap.MarshalMerchantManifest(manifest)
	if err != nil {
		return fmt.Errorf("marshal merchant config: %w", err)
	}
	if path := strings.TrimSpace(opts.out); path != "" {
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		log.WithField("file", path).Info("dump-merchant-config: wrote merchant config YAML")
		return nil
	}
	_, err = cmd.OutOrStdout().Write(encoded)
	return err
}

// applyStartupBootstrap applies the AuthKit authority manifest on the FIRST
// server start only. Catalog files are never reconciled from normal server
// startup (explicit CLI/init-job); the MODE-1 merchant manifest converges
// separately EVERY boot (#847, serverboot.ReconcileBootMerchantManifest).
func applyStartupBootstrap(ctx context.Context, cfg *config.Config, a *app.App) error {
	path := resolveBootstrapManifestPath(cfg)
	if path == "" {
		return nil
	}
	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("startup bootstrap: control plane not attached (#469: it is mandatory in standalone mode)")
	}

	manifest, err := akembedded.LoadBootstrapManifestFile(path)
	if err != nil {
		return err
	}
	log.WithField("file", path).Info("startup bootstrap: first run — applying AuthKit authority manifest")
	if err := applyAuthKitAuthorityManifest(ctx, a, manifest, log.StandardLogger().Out, authkit.BootstrapReconcileOptions{StartupOnly: true, Name: "openrails"}); err != nil {
		return err
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

// applyAuthKitAuthorityManifest reconciles AuthKit-owned standalone authority.
// It intentionally does not touch OpenRails merchant config, secrets, catalog,
// provider state, or remote rails.
func applyAuthKitAuthorityManifest(ctx context.Context, a *app.App, manifest authkit.BootstrapManifest, out io.Writer, opts authkit.BootstrapReconcileOptions) error {
	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("AuthKit authority bootstrap: control plane not attached")
	}
	if cp.Core() == nil {
		return fmt.Errorf("AuthKit authority bootstrap: core service unavailable")
	}
	res, err := cp.Core().ApplyBootstrapManifest(ctx, manifest, opts)
	if err != nil {
		return fmt.Errorf("apply AuthKit authority bootstrap: %w", err)
	}
	return json.NewEncoder(out).Encode(res)
}

// applyPushMerchantConfigManifest provisions OpenRails merchants declared by the
// merchant config manifest: permission-group + optional host-app issuer-as-owner,
// merchant row, provider secrets, and profile (#527). It intentionally does not
// touch catalog/provider state.
func applyPushMerchantConfigManifest(ctx context.Context, cfg *config.Config, a *app.App, manifest *bootstrap.BillingConfig, out io.Writer, dryRun bool, reconcileOpts bootstrap.MerchantManifestReconcileOptions) error {
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
