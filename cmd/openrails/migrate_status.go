package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/migrate"
)

var errMigrationStatusDrift = errors.New("openrails migration status is not exact")

func newMigrateStatusCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Compare embedded Postgres migrations with the applied ledger",
		Long:  "Reports applied and pending migrations and every ledger discrepancy; exits non-zero unless the ledger matches the embedded chain exactly.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			status, err := migrate.PostgresStatus(cmd.Context(), cfg)
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
			if len(status.Pending) > 0 || len(status.Discrepancies) > 0 {
				cmd.Root().SilenceUsage = true
				return errMigrationStatusDrift
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	return cmd
}
