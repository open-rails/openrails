package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

type providerRefreshReceipt struct {
	JobID         int64              `json:"job_id"`
	MerchantID    billing.MerchantID `json:"merchant_id"`
	RiverSchema   string             `json:"river_schema"`
	Queue         string             `json:"queue"`
	AlreadyQueued bool               `json:"already_queued"`
}

func newProviderRefreshCmd() *cobra.Command {
	var merchant string
	cmd := &cobra.Command{
		Use:   "provider-refresh",
		Short: "Queue a provider refresh for one merchant and print its River job ID",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
				mid, err := resolveCLIMerchant(ctx, database, merchant)
				if err != nil {
					return err
				}
				cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
				if err := bindCLIRiverProducer(ctx, cfg, database); err != nil {
					return err
				}
				result, err := riverjobs.RequestProviderRefresh(ctx, database, mid)
				if err != nil {
					return err
				}
				return printJSON(cmd.OutOrStdout(), providerRefreshReceipt{
					JobID:         result.Job.ID,
					MerchantID:    mid,
					RiverSchema:   config.RiverSchemaName(cfg),
					Queue:         result.Job.Queue,
					AlreadyQueued: result.UniqueSkippedAsDuplicate,
				})
			})
		},
	}
	cmd.Flags().StringVar(&merchant, "merchant", "", "Merchant public name or id:<uuid> (required)")
	_ = cmd.MarkFlagRequired("merchant")
	return cmd
}
