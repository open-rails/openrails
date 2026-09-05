package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

type catalogDumpOptions struct {
	merchant         string
	unboundMerchants bool
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
	cmd.Flags().BoolVar(&opts.unboundMerchants, "unbound-merchants", false, "Resolve only host-local merchants without an AuthKit group binding")
	cmd.Flags().StringVar(&opts.merchant, "slug", "", "merchant slug to dump")
	return cmd
}

func runDumpCatalog(cmd *cobra.Command, opts catalogDumpOptions) error {
	slug := strings.ToLower(strings.TrimSpace(opts.merchant))
	if slug == "" {
		return fmt.Errorf("--slug is required")
	}
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	var authority merchant.NameAuthority
	if !opts.unboundMerchants {
		_, configuredAuthority, close, err := openCLINameDirectory(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		defer close()
		authority = configuredAuthority
	}
	return embedded.DumpMerchantCatalog(cmd.Context(), embedded.CatalogDumpOptions{
		Config:        cfg,
		NameAuthority: authority,
		Merchant:      slug,
		Out:           cmd.OutOrStdout(),
	})
}
