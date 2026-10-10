package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

const defaultCatalogManifestPath = "/etc/openrails/catalog.yaml"

// catalogOptions selects the operator file and merchant; mutation intent,
// including any identity and precondition, is in the application document.
type catalogOptions struct {
	file             string
	merchant         string
	merchantManifest string
	forceConflicts   bool
}

// newApplyCatalogCmd executes the same atomic batch as the merchant Client
// through trusted local operator authority, without starting an HTTP server.
func newApplyCatalogCmd() *cobra.Command {
	opts := catalogOptions{file: defaultCatalogManifestPath}
	cmd := &cobra.Command{
		Use:   "apply-catalog",
		Short: "Apply one catalog document to an explicitly selected merchant",
		Long: "Loads one catalog document, applies every product, price and meter in it that no edit contests, " +
			"and prints what changed and what it skipped. An object with a field an edit set differently is skipped " +
			"whole and the command exits non-zero; --force-conflicts overwrites those fields instead. " +
			"A document that applied whole is remembered by its content hash: applying it again changes nothing, " +
			"even after later edits. A prior document is not a rollback. " +
			"Omitted items are kept unless the document sets prune: true, which archives only those it alone set.",
		Args: validateCatalogArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runApplyCatalog(cmd, opts)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&opts.file, "file", "f", defaultCatalogManifestPath, "catalog manifest YAML file")
	flags.StringVar(&opts.merchant, "merchant", "", "merchant name")
	flags.StringVar(&opts.merchantManifest, "merchant-manifest", "", "host-owned merchant credential snapshot (defaults to the conventional merchant manifest)")
	flags.BoolVar(&opts.forceConflicts, "force-conflicts", false, "overwrite the fields an edit set, and take them")
	return cmd
}

func validateCatalogArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return err
	}
	file, err := cmd.Flags().GetString("file")
	if err != nil {
		return err
	}
	file = strings.TrimSpace(file)
	if file == "" {
		file = defaultCatalogManifestPath
	}
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("read catalog manifest: %w", err)
	}
	return nil
}

func runApplyCatalog(cmd *cobra.Command, opts catalogOptions) error {
	if strings.TrimSpace(opts.file) == "" {
		opts.file = defaultCatalogManifestPath
	}

	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	push := hosttools.CatalogApplyOptions{
		Config:               cfg,
		File:                 opts.file,
		Out:                  cmd.OutOrStdout(),
		Merchant:             opts.merchant,
		MerchantManifestPath: opts.merchantManifest,
		Force:                opts.forceConflicts,
	}
	receipt, err := hosttools.ApplyMerchantCatalog(cmd.Context(), push)
	if err != nil {
		return err
	}
	if n := len(receipt.Conflicts); n > 0 {
		return fmt.Errorf("catalog document skipped %d object(s) an edit changed", n)
	}
	return nil
}
