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
	cmd := &cobra.Command{Use: "catalog", Short: "Export and restore complete catalog history with original identities"}
	cmd.AddCommand(newCatalogExportCmd(), newCatalogImportCmd())
	return cmd
}
func newCatalogExportCmd() *cobra.Command {
	var merchant, out string
	var overwrite bool
	cmd := &cobra.Command{Use: "export --merchant UUID --out catalog.snapshot.yaml", Short: "Export all persisted catalog history as a sealed YAML snapshot", Long: "Preserves archived products and prices, UUIDs, revisions, key movements, provider bindings, meters, rate cards and catalog receipts. Does not export purchases, balances, credentials or provider jobs. Use dump-merchant-catalog for editable active-offer YAML.", Args: cobra.NoArgs}
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
	cmd := &cobra.Command{Use: "import --merchant UUID --in catalog.snapshot.yaml", Short: "Restore a catalog snapshot into an empty catalog for the same merchant", Long: "Restores all catalog IDs and persisted history atomically. Provision the exact merchant, referenced PSP accounts and customer IDs first. Existing catalog state is never replaced. An identical successful import remains a no-op after later changes; this is not a rollback command.", Args: cobra.NoArgs}
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
