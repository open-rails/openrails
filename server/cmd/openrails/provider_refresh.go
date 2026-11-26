package main

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/server/internal/hostconfig"
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
		Long: `Queues one openrails.provider_refresh_requested job that reads every PSP of
the merchant and repairs the local book from qualified provider evidence. Run
it after missed webhooks or a long outage, or when investigating drift; nothing
runs it on a schedule. The config must name the deployment's billing database
and River schema.

The answer is {job_id, merchant_id, river_schema, queue, already_queued};
already_queued is true when a refresh for the merchant is outstanding. Inspect
the job in <river_schema>.river_job by job_id. The job reads each PSP in
bounded windows from its watermark (psp_refresh_watermarks) to five minutes
ago (NMI's by last change), NMI and Stripe with a 24-hour overlap; a watermark
advances only after a complete read. Before each Stripe window a separate read records refunds and
disputes matching existing payments, up to eight days back. Incomplete reads
retry up to five attempts; a new request resumes from the watermarks. A live
CCBill PSP continues in its own openrails.paced_provider_refresh job: DataLink
answers it once an hour, so it reads a day of transactions an hour, then its
roster.

Unverified subscriptions are resolved with the PSP that owns them: a verified
renewal renews; a decline inside the dunning window is past_due, past it
canceled; a live remote with a future billing date lends its period end (never
to past_due); an absent or terminal remote cancels and revokes; anything
inconclusive stays unverified with access kept. The hourly billing pass also
retries these checks. Reads run in every write mode; overwrites and lifecycle
repairs need an armed merchant, the destructive switch and a mode above
readonly. Objects without this book's mark (NMI merchant-defined field 20,
Stripe openrails_book) raise only evidence.quiet_foreign.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
				mid, err := resolveCLIMerchant(ctx, database, merchant)
				if err != nil {
					return err
				}
				cfg := hostconfig.Engine(ctx)
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
