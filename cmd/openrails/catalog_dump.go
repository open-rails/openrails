package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

type catalogDumpOptions struct {
	merchant      string
	applicationID string
}

func newDumpCatalogCmd() *cobra.Command {
	opts := catalogDumpOptions{}
	cmd := &cobra.Command{
		Use:   "dump-merchant-catalog --slug <merchant>",
		Short: "Dump the merchant's active default catalog as a new catalog application",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDumpCatalog(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&opts.merchant, "slug", "", "merchant slug to dump")
	cmd.Flags().StringVar(&opts.applicationID, "application-id", "", "identity for the exported application (default: new UUID)")
	return cmd
}

func runDumpCatalog(cmd *cobra.Command, opts catalogDumpOptions) error {
	slug := strings.ToLower(strings.TrimSpace(opts.merchant))
	if slug == "" {
		return fmt.Errorf("--slug is required")
	}
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	return hosttools.DumpMerchantCatalog(cmd.Context(), hosttools.CatalogDumpOptions{
		Config:        cfg,
		Merchant:      slug,
		ApplicationID: opts.applicationID,
		Out:           cmd.OutOrStdout(),
	})
}
