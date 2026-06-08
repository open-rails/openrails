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

// maybeFirstRunBootstrap auto-applies the unified bootstrap manifest exactly
// once, on the first server start from an empty control plane (#327). On later
// starts (tenants already provisioned) it is a no-op, and operators apply
// manifest edits with `billing bootstrap apply`. It is also a no-op when no
// manifest file is configured/present or the control plane is disabled.
func maybeFirstRunBootstrap(ctx context.Context, cfg *config.Config, a *app.App) error {
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
	slugs := make([]string, 0, len(manifest.Tenants))
	for i := range manifest.Tenants {
		slugs = append(slugs, manifest.Tenants[i].Slug)
	}
	provisioned, err := bootstrap.AnyTenantProvisioned(ctx, cp, slugs)
	if err != nil {
		return fmt.Errorf("first-run check: %w", err)
	}
	if provisioned {
		return nil // already bootstrapped; operators run `bootstrap apply` to apply edits
	}
	log.WithField("file", path).Info("first-run bootstrap: applying unified manifest")
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
		plan, err := catalog.PlanWithOptions(ctx, applier, cat, catalog.PlanOptions{ArchiveMissingProducts: false})
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
		result, err := catalog.Apply(ctx, applier, plan)
		if err != nil {
			return fmt.Errorf("apply %s: %w", name, err)
		}
		fmt.Fprintln(out)
		result.Print(out)
	}
	return nil
}
