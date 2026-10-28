package main

import (
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

// newPruneCmd wires the prune and its run list:
//
//	openrails prune --merchant M [--psp ID|KEY] [--rail ...] [--since ... --until ...] [--expect-rows N]
//	openrails prune list --merchant M
//
// A prune soft-deletes rows, stamping each with its run, so `openrails
// undo-run` restores one made against a bad snapshot in one command.
func newPruneCmd() *cobra.Command {
	var (
		merchantSlug string
		psp          string
		rails        []string
		since        string
		until        string
		format       string
		logDir       string
		manifestPath string
		expectRows   int
	)
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Soft-delete a PSP's local subscriptions and payments its provider no longer lists. Plan-only without --expect-rows",
		Long: "Reads the PSP's current roster and transactions and soft-deletes the local subscriptions and payments attributed " +
			"to that PSP which the provider no longer lists. The provider is never written.\n\n" +
			"Without --expect-rows it is a plan: it reports what it would remove and writes nothing. Applying needs the typed " +
			"confirmation --expect-rows N, equal to the number the plan reported, for one PSP at a time (--psp when several are " +
			"armed). An empty provider roster refuses rather than matching everything. Payments prune only inside a --since/--until " +
			"window the provider listed completely. A subscription that fed the grant ledger, or a payment with protected " +
			"dependents, is skipped. An applied prune is reversed in one step with `openrails undo-run --run <id>`.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg := c.Context().Value(config.ConfigContextKey).(*config.Config)
			mid, err := resolveConfiguredCLIMerchant(c.Context(), cfg, merchantSlug)
			if err != nil {
				return err
			}
			manifest, err := cliMerchantManifest(c.Context(), manifestPath)
			if err != nil {
				return err
			}
			var expect *int
			if c.Flags().Changed("expect-rows") {
				expect = &expectRows
			}
			return hosttools.Prune(c.Context(), hosttools.PruneOptions{
				Config: cfg, MerchantID: mid, Rails: rails, PSP: psp, Since: since, Until: until,
				ExpectRows: expect, Actor: cliActor(), Format: format, LogDir: logDir,
				MerchantManifest: manifest, Out: os.Stdout,
			})
		},
	}
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&psp, "psp", "", "PSP id or key to prune (default: the PSP each armed rail arms from)")
	cmd.Flags().StringSliceVar(&rails, "rail", nil, "Rail(s) to prune: nmi, ccbill, stripe, solana (default: all armed)")
	cmd.Flags().StringVar(&since, "since", "", "Transaction window start (RFC3339 or YYYY-MM-DD); payments prune only inside a window")
	cmd.Flags().StringVar(&until, "until", "", "Transaction window end (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().IntVar(&expectRows, "expect-rows", 0, "Typed confirmation: how many rows the prune removes. Required to apply; refuses if it disagrees with the plan")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	cmd.Flags().StringVar(&logDir, "log-dir", "openrails-prune-logs", "Directory for prune .log files")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "MODE-1 merchant manifest to arm credentials from (default: the conventional /etc/openrails/merchants.yaml when present)")

	var listMerchant, listFormat string
	var limit int
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List this merchant's destructive runs, newest first",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg := c.Context().Value(config.ConfigContextKey).(*config.Config)
			mid, err := resolveConfiguredCLIMerchant(c.Context(), cfg, listMerchant)
			if err != nil {
				return err
			}
			return hosttools.PruneList(c.Context(), hosttools.PruneListOptions{
				Config: cfg, MerchantID: mid, Limit: limit, Format: listFormat, Out: os.Stdout,
			})
		},
	}
	listCmd.Flags().StringVar(&listMerchant, "merchant", "", "Merchant public name or id:<uuid> (required)")
	listCmd.Flags().IntVar(&limit, "limit", 20, "Maximum runs to show")
	listCmd.Flags().StringVar(&listFormat, "format", "table", "Output format: table, json")

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
