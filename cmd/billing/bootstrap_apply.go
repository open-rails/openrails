package main

import (
	"context"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
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

	if len(manifest.Tenants) > 0 {
		if opts.dryRun {
			fmt.Fprintf(out, "tenants: %d declared (dry-run: no tenant mutations)\n", len(manifest.Tenants))
		} else {
			cp := embcp.Get(embeddedApp.App())
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

	svc, err := billingservice.New(embeddedApp.App().Runtime)
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
		plan.Print(out, opts.dryRun)
		if opts.dryRun {
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
