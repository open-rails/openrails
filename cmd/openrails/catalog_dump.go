package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/embedded"
)

type catalogDumpOptions struct {
	merchant string
}

func newDumpCatalogCmd() *cobra.Command {
	opts := catalogDumpOptions{}
	cmd := &cobra.Command{
		Use:   "dump-merchant-catalog --slug <merchant>",
		Short: "Dump a merchant's live catalog as push-merchant-catalog YAML",
		Args:  cobra.NoArgs,
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
	_, authority, close, err := openCLINameDirectory(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	defer close()
	return embedded.DumpMerchantCatalog(cmd.Context(), embedded.CatalogDumpOptions{
		Config:        cfg,
		NameAuthority: authority,
		Merchant:      slug,
		Out:           cmd.OutOrStdout(),
	})
}
