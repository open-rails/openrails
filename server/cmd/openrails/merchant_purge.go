package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/merchants"
)

// newMerchantPurgeCmd purges one merchant: alone it takes and prints the
// purge inventory, with the inventory typed back it purges.
func newMerchantPurgeCmd() *cobra.Command {
	var slug, by, inventory, confirm string
	var expectRows int
	cmd := &cobra.Command{
		Use:   "purge --merchant NAME --by NAME [--inventory RUN --expect-rows N --confirm PHRASE]",
		Short: "Permanently delete a merchant's rows and Vault configuration",
		Long: "Alone it records a purge inventory, what a purge would destroy, and prints it with the confirmation " +
			"phrase; it deletes nothing. With --inventory, --expect-rows and --confirm typed back from that inventory it " +
			"purges: one-way, point-in-time recovery is the only way back. The merchant's Vault configuration is " +
			"deleted by a job queued with the purge, which the worker fleet retries until none remains.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(by) == "" {
				return fmt.Errorf("--by is required")
			}
			apply := inventory != "" || confirm != "" || cmd.Flags().Changed("expect-rows")
			var run billing.RunID
			if apply {
				if inventory == "" || confirm == "" || !cmd.Flags().Changed("expect-rows") {
					return fmt.Errorf("a purge needs --inventory, --expect-rows and --confirm from the inventory: run without them first")
				}
				var err error
				if run, err = billing.ParseRunID(strings.TrimSpace(inventory)); err != nil {
					return fmt.Errorf("--inventory: %w", err)
				}
			}
			return withPurgeMerchants(cmd, func(ctx context.Context, database *db.DB, svc *merchants.Service) error {
				id, err := resolveCLIMerchant(ctx, database, slug)
				if err != nil {
					return err
				}
				if !apply {
					taken, err := svc.TakePurgeInventory(ctx, id)
					if err != nil {
						return err
					}
					return printJSON(cmd.OutOrStdout(), taken)
				}
				purged, err := svc.Purge(ctx, id, billing.PurgeMerchantParams{InventoryID: run, ExpectRows: expectRows, ConfirmPhrase: confirm, Actor: strings.TrimSpace(by)})
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), purged)
			})
		},
	}
	cmd.Flags().StringVar(&slug, "merchant", "", "merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&by, "by", "", "who purges (required)")
	cmd.Flags().StringVar(&inventory, "inventory", "", "the inventory's id, to purge")
	cmd.Flags().IntVar(&expectRows, "expect-rows", 0, "Typed confirmation: the inventory's total_rows")
	cmd.Flags().StringVar(&confirm, "confirm", "", "Typed confirmation: the inventory's confirm_phrase")
	return cmd
}

// withPurgeMerchants opens the merchants service a purge runs on: its
// configuration, the destructive-action gate and a River producer in the
// fleet's schema for the cleanup job.
func withPurgeMerchants(cmd *cobra.Command, run func(context.Context, *db.DB, *merchants.Service) error) error {
	return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
		cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
		schema := config.RiverSchemaName(cfg)
		producer, err := river.NewClient(riverpgxv5.New(database.Pool()), &river.Config{Schema: schema, SkipUnknownJobCheck: true})
		if err != nil {
			return fmt.Errorf("River producer: %w", err)
		}
		if err := database.ValidateRiverJobBinding(ctx, database.Pool(), schema); err != nil {
			return fmt.Errorf("River producer: %w", err)
		}
		database.SetRiverJobInserter(producer)
		opened, err := merchantbootstrap.OpenMerchants(ctx, cfg, database)
		if err != nil {
			return err
		}
		defer opened.Close()
		return run(ctx, database, opened.Service.WithDestructivePolicy(destructive.New(database)).WithJobs(database))
	})
}
