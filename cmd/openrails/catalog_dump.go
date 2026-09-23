package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/pkg/merchant"
)

type catalogDumpOptions struct {
	merchant         string
	applicationID    string
	unboundMerchants bool
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
	cmd.Flags().BoolVar(&opts.unboundMerchants, "unbound-merchants", false, "Resolve only host-local merchants without an AuthKit group binding")
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
	var authority merchant.NameAuthority
	if !opts.unboundMerchants {
		_, configuredAuthority, close, err := openCLINameDirectory(cmd.Context(), cfg)
		if err != nil {
			return err
		}
		defer close()
		authority = configuredAuthority
	}
	return hosttools.DumpMerchantCatalog(cmd.Context(), hosttools.CatalogDumpOptions{
		Config:        cfg,
		NameAuthority: authority,
		Merchant:      slug,
		ApplicationID: opts.applicationID,
		Out:           cmd.OutOrStdout(),
	})
}
