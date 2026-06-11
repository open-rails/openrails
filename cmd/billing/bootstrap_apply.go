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
	"github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	billingservice "github.com/open-rails/openrails/pkg/service"
	"github.com/open-rails/openrails/pkg/tenant"
)

type bootstrapApplyOptions struct {
	file   string
	dryRun bool
}

func newBootstrapCmd() *cobra.Command {
	opts := bootstrapApplyOptions{file: bootstrap.DefaultBootstrapManifestPath}
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Apply declarative OpenRails provisioning manifests",
	}
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Apply tenants and catalog from a unified bootstrap YAML file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runBootstrapApply(cmd, opts)
		},
	}
	apply.Flags().StringVarP(&opts.file, "file", "f", bootstrap.DefaultBootstrapManifestPath, "bootstrap manifest YAML file")
	apply.Flags().BoolVar(&opts.dryRun, "dry-run", false, "validate and print plans without mutating state")
	cmd.AddCommand(apply)
	return cmd
}

func runBootstrapApply(cmd *cobra.Command, opts bootstrapApplyOptions) error {
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
		return fmt.Errorf("config not loaded; bootstrap apply requires --config")
	}

	embeddedApp, err := embedded.New(embedded.Options{Config: cfg})
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() {
		if closeErr := embeddedApp.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("application cleanup failed")
		}
	}()

	if err := embcp.Attach(ctx, embeddedApp.App(), cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}

	return applyBootstrapManifest(ctx, cfg, embeddedApp.App(), manifest, out, opts.dryRun)
}

// startupBootstrapLockKey is the pg_advisory_lock key serializing concurrent
// startup bootstraps (#342). The apply is plan-then-execute with no internal
// transaction, so simultaneous replica cold starts against an empty control
// plane would each plan the same creates and race the inserts (23505 on
// unique_prices_product_amount_cycle). Holding this lock across plan+apply
// makes the second replica plan against the converged state instead.
const startupBootstrapLockKey = int64(0x6f72_626f_6f74) // "orboot"

// applyStartupBootstrap applies the unified bootstrap manifest on EVERY server
// start (#327). The apply is idempotent + additive — tenant data is reconciled
// (upsert; issuers/principals are registered if missing) and catalogs plan with
// ArchiveMissingProducts:false, so nothing is ever removed — so reapplying on
// every boot is safe and lets a mounted bootstrap file converge an existing
// control plane (e.g. register a delegated issuer that was added after the DB
// was first provisioned). No-op when no manifest is configured/present or the
// control plane is disabled. Concurrent replicas serialize on a session-level
// advisory lock (#342); the lock auto-releases if the holder dies.
func applyStartupBootstrap(ctx context.Context, cfg *config.Config, a *app.App) error {
	path := resolveBootstrapManifestPath(cfg)
	if path == "" {
		return nil
	}
	cp := embcp.Get(a)
	if cp == nil {
		return nil // verifier-only mode; nothing to provision
	}
	manifest, err := bootstrap.LoadBootstrapManifest(path)
	if err != nil {
		return err
	}

	// Dedicated connection: pg_advisory_lock is session-scoped, so the lock
	// must be taken and released on the same conn, held for the whole apply.
	conn, err := cp.Pool().Acquire(ctx)
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

	log.WithField("file", path).Info("startup bootstrap: applying unified manifest (idempotent)")
	return applyBootstrapManifest(ctx, cfg, a, manifest, log.StandardLogger().Out, false)
}

// resolveBootstrapManifestPath returns the configured bootstrap manifest path,
// or the conventional default location if that file exists, else "".
func resolveBootstrapManifestPath(cfg *config.Config) string {
	if cfg != nil && cfg.TenantBootstrap != nil {
		if p := strings.TrimSpace(cfg.TenantBootstrap.File); p != "" {
			return p
		}
	}
	if _, err := os.Stat(bootstrap.DefaultBootstrapManifestPath); err == nil {
		return bootstrap.DefaultBootstrapManifestPath
	}
	return ""
}

// applyBootstrapManifest provisions the tenants (issuers + service principals)
// and catalog declared by a unified bootstrap manifest. It is the single apply
// path shared by the `bootstrap apply` CLI and the server's first-run startup
// bootstrap (#327), so both consume the same one manifest schema.
func applyBootstrapManifest(ctx context.Context, cfg *config.Config, a *app.App, manifest *bootstrap.BootstrapManifest, out io.Writer, dryRun bool) error {
	if len(manifest.Tenants) > 0 {
		if dryRun {
			fmt.Fprintf(out, "tenants: %d declared (dry-run: no tenant mutations)\n", len(manifest.Tenants))
		} else {
			cp := embcp.Get(a)
			if cp == nil {
				return fmt.Errorf("tenant bootstrap requires auth.control_plane.enabled")
			}
			if err := bootstrap.ReconcileTenantManifestData(ctx, cfg, cp, manifest.TenantManifest(), bootstrap.TenantManifestReconcileOptions{}); err != nil {
				return fmt.Errorf("tenant bootstrap: %w", err)
			}
			fmt.Fprintf(out, "tenants: %d reconciled\n", len(manifest.Tenants))
		}
	}

	if len(manifest.Catalogs) == 0 {
		return nil
	}

	svc, err := billingservice.New(a.Runtime)
	if err != nil {
		return fmt.Errorf("construct OpenRails service: %w", err)
	}
	applier := catalog.NewServiceApplier(svc)

	for i := range manifest.Catalogs {
		cat, err := manifest.CatalogManifest(i)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(manifest.Catalogs[i].Name)
		if name == "" {
			name = fmt.Sprintf("catalog-%d", i+1)
		}
		catalogCtx, err := contextForBootstrapCatalog(ctx, a, manifest.Catalogs[i].Name)
		if err != nil {
			return err
		}
		plan, err := catalog.PlanWithOptions(catalogCtx, applier, cat, catalog.PlanOptions{ArchiveMissingProducts: false})
		if err != nil {
			return fmt.Errorf("plan %s: %w", name, err)
		}
		fmt.Fprintf(out, "\n%s plan:\n", name)
		plan.Print(out, dryRun)
		if dryRun {
			continue
		}
		if !plan.HasChanges() {
			fmt.Fprintf(out, "\n%s: no changes\n", name)
			continue
		}
		result, err := catalog.Apply(catalogCtx, applier, plan)
		if err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		fmt.Fprintln(out)
		result.Print(out)
	}
	return nil
}

func contextForBootstrapCatalog(ctx context.Context, a *app.App, catalogName string) (context.Context, error) {
	slug := strings.ToLower(strings.TrimSpace(catalogName))
	if slug == "" {
		return ctx, nil
	}
	cp := embcp.Get(a)
	if cp == nil || cp.Pool() == nil {
		return nil, fmt.Errorf("catalog %q declares a tenant name but auth.control_plane.enabled is not available", catalogName)
	}
	var id string
	if err := cp.Pool().QueryRow(ctx, `SELECT id::text FROM billing.tenants WHERE lower(slug) = lower($1)`, slug).Scan(&id); err != nil {
		return nil, fmt.Errorf("resolve catalog tenant %q: %w", slug, err)
	}
	tid, err := tenant.ParseID(id)
	if err != nil {
		return nil, fmt.Errorf("parse catalog tenant %q id: %w", slug, err)
	}
	return tenant.WithID(ctx, tid), nil
}
