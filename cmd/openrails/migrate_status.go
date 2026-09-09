package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/migrate"
)

func newMigrateStatusCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Compare embedded Postgres migrations with the applied ledger",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			status, err := migrate.InspectPostgres(cmd.Context(), cfg)
			if err != nil {
				return fmt.Errorf("migration status failed: %w", err)
			}
			if jsonOutput {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(status); err != nil {
					return fmt.Errorf("encode migration status: %w", err)
				}
			} else if _, err := fmt.Fprint(cmd.OutOrStdout(), status.Report()); err != nil {
				return fmt.Errorf("write migration status: %w", err)
			}
			if !status.Exact {
				cmd.Root().SilenceUsage = true
				return migrate.ErrMigrationStatusDrift
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	return cmd
}
