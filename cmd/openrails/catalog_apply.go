package main

import (
	"context"
	"fmt"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/pkg/catalog"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// catalogApplyOptions holds the flags for `openrails catalog apply`.
type catalogApplyOptions struct {
	file   string
	dryRun bool
}

// newCatalogCmd builds the `openrails catalog` command group and its `apply`
// subcommand — a terraform-style declarative apply of a YAML catalog manifest
// (issue #162). It mirrors cozy-art's sync-product-catalog pipeline:
// load -> validate -> plan -> print -> (dry-run? stop : apply).
//
// The command runs in-process: it bootstraps the app runtime and drives
// *service.Service directly, which is the same catalog facade embedded hosts use.
func newCatalogCmd() *cobra.Command {
	opts := catalogApplyOptions{}
	catalogCmd := &cobra.Command{
		Use:   "catalog",
		Short: "Declarative catalog-as-code (terraform-style YAML apply)",
	}
	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a YAML catalog manifest into OpenRails (and all configured providers)",
		Long: "Loads a declarative catalog manifest (tier_groups > products > prices), computes a " +
			"terraform-style plan against the live catalog, prints it, and — unless --dry-run — " +
			"converges OpenRails onto it. Products are identified by slug; prices by financial " +
			"substance (currency, amount, interval). Undeclared active prices are archived; the " +
			"per-product/price providers list fans the apply out to Stripe/NMI/CCBill/Solana.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCatalogApply(cmd, opts)
		},
	}
	flags := applyCmd.Flags()
	flags.StringVarP(&opts.file, "file", "f", "", "catalog manifest YAML file (required)")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "compute and print the plan without mutating anything")
	_ = applyCmd.MarkFlagRequired("file")

	catalogCmd.AddCommand(applyCmd)
	return catalogCmd
}

func runCatalogApply(cmd *cobra.Command, opts catalogApplyOptions) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	manifest, err := catalog.Load(opts.file)
	if err != nil {
		return err
	}

	applier, cleanup, err := buildApplier(ctx, cmd, opts)
	if err != nil {
		return err
	}
	if cleanup != nil {
		defer cleanup()
	}

	plan, err := catalog.Plan(ctx, applier, manifest)
	if err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	plan.Print(out, opts.dryRun)

	if opts.dryRun {
		if !plan.HasChanges() {
			fmt.Fprintln(out, "\nno changes — catalog is up to date")
		}
		return nil
	}
	if !plan.HasChanges() {
		fmt.Fprintln(out, "\nno changes — catalog is up to date")
		return nil
	}

	result, err := catalog.Apply(ctx, applier, plan)
	if err != nil {
		return fmt.Errorf("apply: %w", err)
	}
	fmt.Fprintln(out)
	result.Print(out)
	return nil
}

// buildApplier returns an in-process Applier plus a cleanup func that closes the
// bootstrapped app runtime.
func buildApplier(_ context.Context, cmd *cobra.Command, opts catalogApplyOptions) (catalog.Applier, func(), error) {
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil {
		return nil, nil, fmt.Errorf("config not loaded; in-process mode requires --config")
	}
	application, err := app.Bootstrap(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("bootstrap application: %w", err)
	}
	cleanup := func() {
		if closeErr := application.Close(context.Background()); closeErr != nil {
			log.WithError(closeErr).Error("application cleanup failed")
		}
	}
	svc, err := billingservice.New(application.Runtime)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("construct OpenRails service: %w", err)
	}
	return catalog.NewServiceApplier(svc), cleanup, nil
}
