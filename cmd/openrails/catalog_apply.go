package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/embedded"
)

const defaultCatalogManifestPath = "/etc/openrails/catalog.yaml"

// catalogOptions holds the flags for `openrails push-merchant-catalog`.
type catalogOptions struct {
	file      string
	dryRun    bool
	insert    bool
	overwrite bool
	prune     bool
}

// newPushCatalogCmd builds the `openrails push-merchant-catalog` command — a terraform-style
// declarative apply of a YAML catalog manifest
// (issue #162). It mirrors cozy-art's sync-product-catalog pipeline:
// load -> validate -> plan -> print -> (dry-run? stop : apply).
//
// The command runs in-process through a catalog-sized runtime: Postgres plus the
// catalog/provider facades, without starting the server runtime.
func newPushCatalogCmd() *cobra.Command {
	opts := catalogOptions{file: defaultCatalogManifestPath}
	cmd := &cobra.Command{
		Use:     "push-merchant-catalog",
		Aliases: []string{"push-catalog"},
		Short:   "Push a YAML merchant catalog manifest into OpenRails and configured providers",
		Long: "Loads a declarative catalog manifest (catalogs[] > products > prices), " +
			"computes a terraform-style plan per merchant, and prints it. A bare command is plan-only. " +
			"Mutation classes are explicit and compose: --insert creates missing products/prices/provider objects; " +
			"--overwrite updates existing OpenRails-owned catalog rows; --prune archives OpenRails-owned extras. " +
			"Products are identified by key within their merchant; prices by financial substance (currency, amount, interval).",
		Args: validateCatalogArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPushCatalog(cmd, opts)
		},
	}
	flags := cmd.Flags()
	flags.StringVarP(&opts.file, "file", "f", defaultCatalogManifestPath, "catalog manifest YAML file")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "deprecated alias for the default plan-only behavior")
	_ = flags.MarkHidden("dry-run")
	flags.BoolVar(&opts.insert, "insert", false, "Create missing OpenRails/provider catalog objects from the manifest")
	flags.BoolVar(&opts.overwrite, "overwrite", false, "Update existing OpenRails-owned catalog objects from the manifest")
	flags.BoolVar(&opts.prune, "prune", false, "archive OpenRails-owned provider objects absent from the local catalog; foreign provider objects are never touched")
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

func runPushCatalog(cmd *cobra.Command, opts catalogOptions) error {
	if strings.TrimSpace(opts.file) == "" {
		opts.file = defaultCatalogManifestPath
	}

	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	return embedded.PushMerchantCatalog(cmd.Context(), embedded.CatalogPushOptions{
		Config:    cfg,
		File:      opts.file,
		Out:       cmd.OutOrStdout(),
		DryRun:    opts.dryRun,
		Insert:    opts.insert,
		Overwrite: opts.overwrite,
		Prune:     opts.prune,
	})
}
