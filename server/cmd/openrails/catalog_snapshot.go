package main

import (
	"fmt"
	"io"
	"os/signal"
	"syscall"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/spf13/cobra"
)

func newCatalogCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "catalog", Short: "Apply catalog documents; export and restore complete catalog history"}
	cmd.AddCommand(newCatalogApplyCmd(), newCatalogExportCmd(), newCatalogImportCmd())
	return cmd
}
func newCatalogExportCmd() *cobra.Command {
	var merchant, out string
	var overwrite bool
	cmd := &cobra.Command{Use: "export --merchant UUID --out catalog.snapshot.yaml", Short: "Export all persisted catalog history as a sealed YAML snapshot", Long: `Writes a backup of the merchant's catalog from a consistent snapshot of the
configured database: a versioned kind: catalog_snapshot YAML with named rows
and a SHA-256 digest, published atomically to a private file (an existing
file needs --overwrite).

It keeps every row of the catalog tables (products, product_entitlements,
prices, price_key_movements, price_psp_bindings, catalog_meters,
catalog_limits, catalog_product_limits, catalog_rate_cards,
catalog_field_owners, catalog_applications) and the catalog revision:
archived products and prices, UUIDs and revisions, so every purchase keeps its
original price. Overwritten meter and rate-card definitions were never stored
and cannot be recovered. It exports no purchases, balances, credentials or
provider jobs: use billing export for the whole book, and
dump-merchant-catalog for an editable document. Artifacts are bounded at
64 MiB.`, Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		mid, err := parseBillingArchiveMerchant(merchant)
		if err != nil {
			return err
		}
		if err := validateArchivePath(out, "out"); err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
		database, err := openCLIDB(ctx, cfg)
		if err != nil {
			return err
		}
		defer database.Close()
		err = writeBillingArchive(out, overwrite, func(writer io.Writer) error { return merchantarchive.ExportCatalog(ctx, database, mid, writer) })
		if err != nil {
			return fmt.Errorf("export catalog: %w", err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "exported catalog merchant=%s file=%s\n", mid, out)
		return err
	}
	cmd.Flags().StringVar(&merchant, "merchant", "", "Exact source merchant UUID")
	cmd.Flags().StringVar(&out, "out", "", "Snapshot destination (required; written atomically)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Replace an existing file only after successful export")
	return cmd
}
func newCatalogImportCmd() *cobra.Command {
	var merchant, in string
	cmd := &cobra.Command{Use: "import --merchant UUID --in catalog.snapshot.yaml", Short: "Restore a catalog snapshot into an empty catalog for the same merchant", Long: `Restores a catalog snapshot's IDs and history in one transaction, into an
empty catalog (revision zero) of the merchant whose UUID the artifact names;
it never merges, replaces or deletes. Provision that merchant first (for a new
local destination, billing prepare-target), and the PSPs its bindings
reference with their original UUID, rail, environment and account id: an
ordinary PSP's UUID derives from those, so declaring the same PSP recreates
it. IDs are never remapped. Aliases, tags, duplicate keys and unknown fields
are refused.

An identical artifact already imported is a no-op even after later edits; a
different one is refused. This is not a rollback. Nothing contacts a provider,
charges, grants access or schedules jobs.`, Args: cobra.NoArgs}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		mid, err := parseBillingArchiveMerchant(merchant)
		if err != nil {
			return err
		}
		input, err := openBillingArchiveInput(in)
		if err != nil {
			return err
		}
		defer input.Close()
		ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
		database, err := openCLIDB(ctx, cfg)
		if err != nil {
			return err
		}
		defer database.Close()
		result, err := merchantarchive.RestoreCatalog(ctx, database, mid, input)
		if err != nil {
			return fmt.Errorf("import catalog: %w", err)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "imported catalog merchant=%s digest=%s rows=%d already_imported=%t\n", mid, result.Digest, result.Rows, result.Replayed)
		return err
	}
	cmd.Flags().StringVar(&merchant, "merchant", "", "Exact destination merchant UUID; must match snapshot")
	cmd.Flags().StringVar(&in, "in", "", "Sealed catalog YAML snapshot to restore")
	return cmd
}
