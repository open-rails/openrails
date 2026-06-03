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

// catalogApplyOptions holds the flags for `billing catalog apply`.
type catalogApplyOptions struct {
	file     string
	dryRun   bool
	apiURL   string
	apiToken string
}

// newCatalogCmd builds the `billing catalog` command group and its `apply`
// subcommand — a terraform-style declarative apply of a YAML catalog manifest
// (issue #162). It mirrors cozy-art's sync-product-catalog pipeline:
// load -> validate -> plan -> print -> (dry-run? stop : apply).
//
// Mode selection:
//   - no --api-url   => in-process facade mode: bootstrap the app runtime and
//     drive *service.Service directly (same path embedded hosts use).
//   - --api-url set  => HTTP mode: drive a remote standalone OpenRails over its
//     admin catalog HTTP API, authenticating with --api-token as an
//     operator-admin bearer token. No DB/config needed locally.
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
	flags.StringVar(&opts.apiURL, "api-url", "", "remote OpenRails base URL incl. api prefix (e.g. https://host/billing/v1); empty = in-process mode")
	flags.StringVar(&opts.apiToken, "api-token", "", "operator-admin bearer token for --api-url (HTTP) mode")
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

// buildApplier selects in-process vs HTTP mode and returns an Applier plus an
// optional cleanup func (non-nil only in in-process mode, where it closes the
// bootstrapped app runtime).
func buildApplier(_ context.Context, cmd *cobra.Command, opts catalogApplyOptions) (catalog.Applier, func(), error) {
	if opts.apiURL != "" {
		return catalog.NewHTTPApplier(opts.apiURL, opts.apiToken), nil, nil
	}

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
		return nil, nil, fmt.Errorf("construct billing service: %w", err)
	}
	return catalog.NewServiceApplier(svc), cleanup, nil
}
