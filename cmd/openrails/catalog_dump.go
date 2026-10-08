package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

type catalogDumpOptions struct {
	merchant string
}

func newDumpCatalogCmd() *cobra.Command {
	opts := catalogDumpOptions{}
	cmd := &cobra.Command{
		Use:   "dump-merchant-catalog --slug <merchant>",
		Short: "Dump the merchant's active default catalog as a YAML application",
		Long: "Exports unarchived products and prices, meters, and default product rate cards. " +
			"Includes prepaid credit grants and customer-selected amount ranges. " +
			"Archived revisions, immutable IDs, purchases and balances require billing export. " +
			"Provider links refer to the selected merchant's configured PSP accounts. " +
			"Reimport with apply-catalog; an already applied document remains a replay, not a rollback.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDumpCatalog(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.merchant, "slug", "", "merchant slug to dump")
	return cmd
}

func runDumpCatalog(cmd *cobra.Command, opts catalogDumpOptions) error {
	slug := strings.ToLower(strings.TrimSpace(opts.merchant))
	if slug == "" {
		return fmt.Errorf("--slug is required")
	}
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	return hosttools.DumpMerchantCatalog(cmd.Context(), hosttools.CatalogDumpOptions{
		Config:   cfg,
		Merchant: slug,
		Out:      cmd.OutOrStdout(),
	})
}
