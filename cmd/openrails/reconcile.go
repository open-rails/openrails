package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/embedded"
)

// newPullProviderCmd wires the #107/#511 provider-pull CLI:
//
//	openrails pull-provider [--rail=... --since=... --until=...] [--insert|--overwrite|--prune]
//	openrails pull-provider report [--run=ID] latest/specified run report
//
// Reconciliation is manual-only by design (no scheduled runs). The remote
// rails are NEVER mutated: mutation flags converge local state only.
// Remote-provider writes are queued by the operation that requests them through
// the provider intent ledger, not by provider pull.
func newPullProviderCmd() *cobra.Command {
	var (
		providers           []string
		since               string
		until               string
		format              string
		merchantSlug        string
		railMerchantAccount string
		runIDStr            string
		logDir              string
		manifestPath        string
		insert              bool
		overwrite           bool
		prune               bool
	)

	cmd := &cobra.Command{
		Use:   "pull-provider",
		Short: "Pull provider-observed state into the local mirror, then converge (#511). Plan-only unless mutation flags are set.",
		Long: "Pull provider-observed truth (subscriptions, payments, refunds, vault) into the local mirror, " +
			"then run a one-shot Converge(merchant).\n\n" +
			"Safety-first: a bare `pull-provider` is plan-only — it discovers every PULL-class divergence and logs " +
			"the changes it WOULD make, writing nothing. Mutation classes are explicit and compose: `--insert` imports " +
			"provider-observed records missing locally; `--overwrite` updates existing local mirror rows from provider " +
			"truth; `--prune` deletes eligible local subscriptions/payments attributed to the pulled provider account " +
			"that are ABSENT from the provider source. The remote rails are NEVER mutated.",
		RunE: func(c *cobra.Command, _ []string) error {
			return runPullProvider(c, providers, railMerchantAccount, since, until, format, merchantSlug, logDir, manifestPath, insert, overwrite, prune)
		},
	}
	cmd.Flags().StringSliceVar(&providers, "rail", nil, "Rail(s) to pull: nmi, ccbill, stripe, solana (default: all configured)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "MODE-1 (#723) merchant manifest to arm credentials from (default: the conventional /etc/openrails/merchants.yaml when present)")
	cmd.Flags().StringVar(&railMerchantAccount, "provider-account", "", "Provider account UUID to pull explicitly (requires matching configured credentials)")
	cmd.Flags().StringVar(&since, "since", "", "Transaction window start (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&until, "until", "", "Transaction window end (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (required)")
	cmd.Flags().StringVar(&logDir, "log-dir", "openrails-pull-provider-logs", "Directory for pull-provider .log files")
	cmd.Flags().BoolVar(&insert, "insert", false, "Insert provider-observed records that are missing locally")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite existing local mirror rows from provider-observed truth")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete eligible local subscriptions/payments for the pulled provider account that are ABSENT from the provider source")

	reportCmd := &cobra.Command{
		Use:   "report",
		Short: "Show a run's summary, open findings, and dunning forensics (latest run by default merchant)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileReport(c, runIDStr, format, merchantSlug)
		},
	}
	reportCmd.Flags().StringVar(&runIDStr, "run", "", "Run ID (default: the latest run)")
	reportCmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	reportCmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (required)")

	cmd.AddCommand(reportCmd)
	return cmd
}

func runPullProvider(cmd *cobra.Command, providerNames []string, railMerchantAccountStr, sinceStr, untilStr, format, merchantSlug, logDir, manifestPath string, insert, overwrite, prune bool) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	return embedded.PullProvider(cmd.Context(), embedded.PullProviderOptions{
		Config:               cfg,
		MerchantSlug:         merchantSlug,
		Providers:            providerNames,
		RailMerchantAccount:  railMerchantAccountStr,
		Since:                sinceStr,
		Until:                untilStr,
		Format:               format,
		LogDir:               logDir,
		MerchantManifestPath: manifestPath,
		Insert:               insert,
		Overwrite:            overwrite,
		Prune:                prune,
		Out:                  os.Stdout,
	})
}

func runReconcileReport(cmd *cobra.Command, runIDStr, format, merchantSlug string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	return embedded.PullProviderReport(cmd.Context(), embedded.PullProviderReportOptions{
		Config:       cfg,
		MerchantSlug: merchantSlug,
		RunID:        runIDStr,
		Format:       format,
		Out:          os.Stdout,
	})
}
