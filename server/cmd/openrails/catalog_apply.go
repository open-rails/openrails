package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

// catalogApplyOptions selects the merchant; mutation intent, including any
// identity and precondition, is in the application document.
type catalogApplyOptions struct {
	merchant         string
	merchantManifest string
	forceConflicts   bool
}

// newCatalogApplyCmd executes the same atomic batch as the merchant Client
// through trusted local operator authority, without starting an HTTP server.
func newCatalogApplyCmd() *cobra.Command {
	var opts catalogApplyOptions
	cmd := &cobra.Command{
		Use:   "apply FILE --merchant NAME",
		Short: "Apply one catalog document to an explicitly selected merchant",
		Long: "Loads one catalog document, applies every product, price and meter in it that no edit contests, " +
			"and prints what changed and what it skipped. An object with a field an edit set differently is skipped " +
			"whole and the command exits non-zero; --force-conflicts overwrites those fields instead. " +
			"A document that applied whole is remembered by its content hash: applying it again changes nothing, " +
			"even after later edits. A prior document is not a rollback. " +
			"Omitted items are kept unless the document sets prune: true, which archives only those it alone set. " +
			"Starting OpenRails never applies a catalog: this command, the API or the console does.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCatalogApply(cmd, strings.TrimSpace(args[0]), opts)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.merchant, "merchant", "", "merchant name")
	flags.StringVar(&opts.merchantManifest, "merchant-manifest", "", "host-owned merchant credential snapshot (defaults to the conventional merchant manifest)")
	flags.BoolVar(&opts.forceConflicts, "force-conflicts", false, "overwrite the fields an edit set, and take them")
	return cmd
}

func runCatalogApply(cmd *cobra.Command, file string, opts catalogApplyOptions) error {
	if file == "" {
		return fmt.Errorf("catalog file is required")
	}
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	manifest, err := cliMerchantManifest(cmd.Context(), opts.merchantManifest)
	if err != nil {
		return err
	}
	receipt, err := hosttools.ApplyMerchantCatalog(cmd.Context(), hosttools.CatalogApplyOptions{
		Config:           cfg,
		File:             file,
		Merchant:         opts.merchant,
		MerchantManifest: manifest,
		Force:            opts.forceConflicts,
	})
	if err != nil {
		return err
	}
	if err := openrails.WriteCatalogReceipt(cmd.OutOrStdout(), receipt); err != nil {
		return fmt.Errorf("write catalog receipt (application committed): %w", err)
	}
	if n := len(receipt.Conflicts); n > 0 {
		return fmt.Errorf("catalog document skipped %d object(s) an edit changed; --force-conflicts overwrites them", n)
	}
	return nil
}
