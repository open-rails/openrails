package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/migrate"
)

var errAccessCutoverUnapproved = errors.New("the product access cutover has unapproved access changes")

// newAccessCutoverCmd is the preflight of the cutover to product access: it
// lists exactly whose access the cutover changes, and --approve records that
// list so `openrails migrate up` applies it.
func newAccessCutoverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "access-cutover",
		Short: "Review the cutover from per-key entitlements to product access",
	}
	var approve string
	var jsonOutput bool
	preflight := &cobra.Command{
		Use:   "preflight",
		Short: "List every customer whose access the cutover changes; --approve NAME approves them",
		Long: "Applies the migrations before the cutover, dry-runs it for every merchant and lists each customer and key it changes " +
			"(lost: their product no longer grants it; gained: their product added it) and the windows it cannot carry. " +
			"The cutover refuses any change not approved. Exits non-zero while a change is unapproved.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
			pool, err := pgxpool.New(cmd.Context(), config.DBConnectionString(cfg.DB))
			if err != nil {
				return fmt.Errorf("open postgres: %w", err)
			}
			defer pool.Close()
			report, err := migrate.AccessCutoverPreflight(cmd.Context(), pool, config.SchemaName(cfg), strings.TrimSpace(approve))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if jsonOutput {
				if err := json.NewEncoder(out).Encode(report); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(out, "%d access changes, %d unapproved\n", len(report.Changes), report.Unapproved())
				for _, c := range report.Changes {
					fmt.Fprintf(out, "  merchant %s customer %s %-6s %q approved=%t\n", c.MerchantID, c.CustomerID, c.Change, c.Entitlement, c.Approved)
				}
				for _, n := range report.Notes {
					fmt.Fprintf(out, "  note %s: merchant %s customer %s %s %s %v\n", n.Note, n.MerchantID, n.CustomerID, n.SourceType, n.SourceID, n.Entitlements)
				}
			}
			if report.Unapproved() > 0 {
				cmd.Root().SilenceUsage = true
				return errAccessCutoverUnapproved
			}
			return nil
		},
	}
	preflight.Flags().StringVar(&approve, "approve", "", "approve every listed change, recorded as approved by this name")
	preflight.Flags().BoolVar(&jsonOutput, "json", false, "emit machine-readable JSON")
	cmd.AddCommand(preflight)
	return cmd
}
