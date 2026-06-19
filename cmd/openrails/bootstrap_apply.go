package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

type pushBootstrapOptions struct {
	file   string
	dryRun bool
}

func newPushBootstrapCmd() *cobra.Command {
	opts := pushBootstrapOptions{file: bootstrap.DefaultBootstrapManifestPath}
	cmd := &cobra.Command{
		Use:     "push-merchant-config",
		Aliases: []string{"push-bootstrap"},
		Short:   "Apply AuthKit authority and OpenRails merchant configuration from YAML",
		Args:    validatePushBootstrapArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPushBootstrap(cmd, opts)
		},
	}
	cmd.Flags().StringVarP(&opts.file, "file", "f", bootstrap.DefaultBootstrapManifestPath, "bootstrap manifest YAML file")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "validate and print plans without mutating state")
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

	return applyPushBootstrapManifest(ctx, cfg, application, manifest, out, opts.dryRun)
}

// startupBootstrapLockKey is the pg_advisory_lock key serializing concurrent
// startup auth/merchant bootstraps (#342). The apply is plan-then-execute with
// no internal transaction, so simultaneous replica cold starts against an empty
// control plane would each plan the same creates and race the inserts. Holding
// this lock across plan+apply makes the second replica plan against the
// converged state instead.
const startupBootstrapLockKey = int64(0x6f72_626f_6f74) // "orboot"

// applyStartupBootstrap applies the bootstrap manifest on EVERY server
// start (#327). The apply is idempotent + additive: merchant data is reconciled
// and AuthKit authority is ensured. Catalog state is never pushed from startup;
// operators use `openrails push-merchant-catalog` for that.
func applyStartupBootstrap(ctx context.Context, cfg *config.Config, a *app.App) error {
	path := resolveBootstrapManifestPath(cfg)
	if path == "" {
		return nil
	}
	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("startup bootstrap: control plane not attached (#469: it is mandatory in standalone mode)")
	}
	manifest, err := bootstrap.LoadBootstrapManifest(path)
	if err != nil {
		return err
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

	log.WithField("file", path).Info("startup bootstrap: applying bootstrap manifest (idempotent)")
	return applyPushBootstrapManifest(ctx, cfg, a, manifest, log.StandardLogger().Out, false)
}

// resolveBootstrapManifestPath returns the conventional bootstrap manifest
// location when that file exists, else "".
func resolveBootstrapManifestPath(_ *config.Config) string {
	if _, err := os.Stat(bootstrap.DefaultBootstrapManifestPath); err == nil {
		return bootstrap.DefaultBootstrapManifestPath
	}
	return ""
}

// applyPushBootstrapManifest provisions AuthKit authority and OpenRails merchant
// definitions. It intentionally does not touch catalog/provider state.
func applyPushBootstrapManifest(ctx context.Context, cfg *config.Config, a *app.App, manifest *bootstrap.BootstrapManifest, out io.Writer, dryRun bool) error {
	if manifest.HasAuthBootstrap() {
		cp := embcp.Get(a)
		if cp == nil || cp.Core() == nil {
			return fmt.Errorf("auth bootstrap: control plane not attached")
		}
		result, err := cp.Core().ReconcileBootstrapManifest(ctx, *manifest.Auth, authcore.FileBootstrapTokenStore{}, authcore.BootstrapReconcileOptions{DryRun: dryRun})
		if err != nil {
			return fmt.Errorf("auth bootstrap: %w", err)
		}
		printAuthBootstrapResult(out, result)
	}

	if len(manifest.Merchants) == 0 {
		return nil
	}
	if dryRun {
		fmt.Fprintf(out, "merchants: %d declared (dry-run: no merchant mutations)\n", len(manifest.Merchants))
		return nil
	}

	cp := embcp.Get(a)
	if cp == nil {
		return fmt.Errorf("merchant bootstrap: control plane not attached")
	}
	if err := bootstrap.ReconcileMerchantManifestData(ctx, cfg, cp, manifest.MerchantManifest(), bootstrap.MerchantManifestReconcileOptions{}); err != nil {
		return fmt.Errorf("merchant bootstrap: %w", err)
	}
	fmt.Fprintf(out, "merchants: %d reconciled\n", len(manifest.Merchants))
	return nil
}

func printAuthBootstrapResult(out io.Writer, result authcore.BootstrapManifestResult) {
	mode := "reconciled"
	if result.DryRun {
		mode = "planned (dry-run)"
	}
	fmt.Fprintf(out, "auth: %s users=%d updated=%d passwords_set=%d passwords_kept=%d global_roles=%d global_role_assignments=%d orgs=%d issuers=%d roles=%d memberships=%d service_tokens_minted=%d service_tokens_kept=%d\n",
		mode,
		result.UsersCreated,
		result.UsersUpdated,
		result.PasswordsSet,
		result.PasswordsKept,
		result.GlobalRoles,
		result.GlobalRoleAssignments,
		result.OrgManifest.Orgs,
		result.OrgManifest.Issuers,
		result.OrgManifest.Roles,
		result.OrgManifest.Memberships,
		result.OrgManifest.TokensMinted,
		result.OrgManifest.TokensKept,
	)
}
