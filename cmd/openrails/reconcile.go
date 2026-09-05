package main

import (
	"os"
	"strings"

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
		providers    []string
		since        string
		until        string
		format       string
		merchantSlug string
		psp          string
		runIDStr     string
		logDir       string
		manifestPath string
		insert       bool
		overwrite    bool
		prune        bool
		expectRows   int
	)

	cmd := &cobra.Command{
		Use:   "pull-provider",
		Short: "Pull provider-observed state into the local mirror, then converge (#511). Plan-only unless mutation flags are set.",
		Long: "Pull provider-observed truth (subscriptions, payments, refunds, vault) into the local mirror, " +
			"then run a one-shot Converge(merchant).\n\n" +
			"Safety-first: a bare `pull-provider` is plan-only — it discovers every PULL-class divergence and logs " +
			"the changes it WOULD make, writing nothing. Mutation classes are explicit and compose: `--insert` imports " +
			"provider-observed records missing locally; `--overwrite` updates existing local mirror rows from provider " +
			"truth; `--prune` deletes eligible local subscriptions/payments attributed to the pulled PSP " +
			"that are ABSENT from the provider source — SOFT-deleted and reversible, never row-deleted (or#858). The remote rails are NEVER mutated.\n\n" +
			"`--prune` alone is a PLAN: it reports what it would remove and writes nothing. Applying needs the typed confirmation " +
			"`--expect-rows N`, which must equal the number the plan reported; an empty provider roster refuses outright rather than " +
			"matching everything. An applied prune is reversible in one step with `openrails undo-run --run <id>`.",
		RunE: func(c *cobra.Command, _ []string) error {
			var expect *int
			if c.Flags().Changed("expect-rows") {
				expect = &expectRows
			}
			return runPullProvider(c, providers, psp, since, until, format, merchantSlug, logDir, manifestPath, insert, overwrite, prune, expect)
		},
	}
	cmd.Flags().StringSliceVar(&providers, "rail", nil, "Rail(s) to pull: nmi, ccbill, stripe, solana (default: all configured)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "MODE-1 (#723) merchant manifest to arm credentials from (default: the conventional /etc/openrails/merchants.yaml when present)")
	cmd.Flags().StringVar(&psp, "provider-account", "", "PSP UUID to pull explicitly (requires matching configured credentials)")
	cmd.Flags().StringVar(&since, "since", "", "Transaction window start (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&until, "until", "", "Transaction window end (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&logDir, "log-dir", "openrails-pull-provider-logs", "Directory for pull-provider .log files")
	cmd.Flags().BoolVar(&insert, "insert", false, "Insert provider-observed records that are missing locally")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite existing local mirror rows from provider-observed truth")
	cmd.Flags().BoolVar(&prune, "prune", false, "Soft-delete eligible local subscriptions/payments for the pulled PSP that are ABSENT from the provider source (plan-only without --expect-rows)")
	cmd.Flags().IntVar(&expectRows, "expect-rows", 0, "Typed confirmation: how many rows --prune should remove. Required to APPLY a prune; refuses if it disagrees with the plan")

	reportCmd := &cobra.Command{
		Use:   "report",
		Short: "Show a run's summary, open findings, and dunning forensics (latest run by default merchant)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileReport(c, runIDStr, format, merchantSlug)
		},
	}
	reportCmd.Flags().StringVar(&runIDStr, "run", "", "Run ID (default: the latest run)")
	reportCmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	reportCmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")

	cmd.AddCommand(reportCmd)
	return cmd
}

func runPullProvider(cmd *cobra.Command, providerNames []string, pspStr, sinceStr, untilStr, format, merchantSlug, logDir, manifestPath string, insert, overwrite, prune bool, expectRows *int) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	mid, err := resolveConfiguredCLIMerchant(cmd.Context(), cfg, merchantSlug)
	if err != nil {
		return err
	}
	directory, authority, close, err := openCLINameDirectory(cmd.Context(), cfg)
	if err != nil {
		return err
	}
	defer close()
	selected, err := directory.Get(cmd.Context(), mid)
	if err != nil {
		return err
	}
	if selected.PermissionGroupID == "" {
		authority = nil
	}
	return embedded.PullProvider(cmd.Context(), embedded.PullProviderOptions{
		NameAuthority:        authority,
		Config:               cfg,
		MerchantID:           mid,
		Providers:            providerNames,
		PSP:                  pspStr,
		Since:                sinceStr,
		Until:                untilStr,
		Format:               format,
		LogDir:               logDir,
		MerchantManifestPath: manifestPath,
		Insert:               insert,
		Overwrite:            overwrite,
		Prune:                prune,
		PruneExpectRows:      expectRows,
		PruneActor:           cliActor(),
		Out:                  os.Stdout,
	})
}

func runReconcileReport(cmd *cobra.Command, runIDStr, format, merchantSlug string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	mid, err := resolveConfiguredCLIMerchant(cmd.Context(), cfg, merchantSlug)
	if err != nil {
		return err
	}
	return embedded.PullProviderReport(cmd.Context(), embedded.PullProviderReportOptions{
		Config:     cfg,
		MerchantID: mid,
		RunID:      runIDStr,
		Format:     format,
		Out:        os.Stdout,
	})
}

// newPruneCmd wires the or#858 inspection surface:
//
//	openrails prune list --merchant <slug>
//
// A prune no longer deletes rows — it soft-deletes them and stamps each one
// with the destructive run that took it, so an operator who pruned against a
// bad snapshot gets the book back with one command. That command is
// `openrails undo-run` (or#859): the reversal is one verb over the whole run
// ledger, kind-dispatched, so nobody can reverse the wrong way round.
func newPruneCmd() *cobra.Command {
	var merchantSlug, format string
	var limit int

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Inspect `pull-provider --prune` runs (or#858); reverse them with `openrails undo-run`",
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List this merchant's destructive runs, newest first",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg := c.Context().Value(config.ConfigContextKey).(*config.Config)
			mid, err := resolveConfiguredCLIMerchant(c.Context(), cfg, merchantSlug)
			if err != nil {
				return err
			}
			return embedded.PruneList(c.Context(), embedded.PruneListOptions{
				Config: cfg, MerchantID: mid, Limit: limit, Format: format, Out: os.Stdout,
			})
		},
	}
	listCmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	listCmd.Flags().IntVar(&limit, "limit", 20, "Maximum runs to show")
	listCmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")

	cmd.AddCommand(listCmd)
	return cmd
}

// cliActor attributes a destructive run to whoever ran it. Best-effort: the
// point is an audit trail, not authentication.
func cliActor() string {
	for _, key := range []string{"OPENRAILS_ACTOR", "SUDO_USER", "USER", "LOGNAME"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "unknown"
}
