package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newConvergeCmd wires the or#859 reversal surface for converge-enforce, the
// sibling of `openrails prune {list,rollback}`:
//
//	openrails converge list     --merchant <slug>
//	openrails converge rollback --merchant <slug> --run <id>
//
// A prune destroys ROWS and reverses by clearing their tombstones. An enforce
// pass destroys row VALUES — an empty NMI roster cancelling 40/40
// subscriptions overwrote status, ended_at, the grace/retry schedule and the
// period bounds, and queued deferred vault deletes behind them. That reverses
// from a captured before-image, not a tombstone, which is why it is a separate
// verb reading the same run ledger.
func newConvergeCmd() *cobra.Command {
	var merchantSlug, runID, format string
	var limit int

	cmd := &cobra.Command{
		Use:   "converge",
		Short: "Inspect and reverse enforcing provider-pull (converge-enforce) runs (or#859)",
	}

	listCmd := &cobra.Command{
		Use:          "list",
		Short:        "List this merchant's converge-enforce runs, newest first",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runConvergeList(c, merchantSlug, limit, format)
		},
	}
	listCmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (required)")
	listCmd.Flags().IntVar(&limit, "limit", 20, "Maximum runs to show")
	listCmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")

	rollbackCmd := &cobra.Command{
		Use:   "rollback",
		Short: "Reverse one converge-enforce run by id — restores every subscription it overwrote",
		Long: "Reverse one converge-enforce run by id.\n\n" +
			"Order matters and is not configurable. The merchant is quiesced first (first-enforce arming cleared,\n" +
			"destructive actions stopped), then the provider writes the run QUEUED BUT HAS NOT SENT are superseded —\n" +
			"before any row is restored, because that step is a race against the intent runner and every millisecond\n" +
			"spent elsewhere is a millisecond it can be lost. Only then are the subscriptions restored from their\n" +
			"before-images, in one transaction.\n\n" +
			"What it will NOT do, on purpose:\n" +
			"  * restore entitlements — Converge recomputes them from the append-only grant log, which no rollback touches\n" +
			"  * touch the ledger, grants, or the intent/webhook/findings logs\n" +
			"  * pretend a provider write that already fired was undone; those are reported as irreversible divergence\n\n" +
			"A rollback is not a complete operation. `rollback -> pull -> converge` is; the command prints the follow-up.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return runConvergeRollback(c, merchantSlug, runID, format)
		},
	}
	rollbackCmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (required)")
	rollbackCmd.Flags().StringVar(&runID, "run", "", "Destructive run id to reverse (required; see `openrails converge list`)")
	rollbackCmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")

	cmd.AddCommand(listCmd, rollbackCmd)
	return cmd
}

func convergeOpenDB(cmd *cobra.Command, merchantSlug string) (*db.DB, merchant.ID, error) {
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil || cfg.DB == nil {
		return nil, merchant.ID{}, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, merchant.ID{}, fmt.Errorf("open postgres: %w", err)
	}
	mid, err := db.ResolveMerchantSlug(cmd.Context(), database.Pool(), strings.TrimSpace(merchantSlug))
	if err != nil {
		_ = database.Close()
		return nil, merchant.ID{}, fmt.Errorf("resolve merchant %q: %w", merchantSlug, err)
	}
	return database, mid, nil
}

func runConvergeList(cmd *cobra.Command, merchantSlug string, limit int, format string) error {
	if limit <= 0 {
		limit = 20
	}
	database, mid, err := convergeOpenDB(cmd, merchantSlug)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	kind := reconcile.DestructiveRunKindConvergeEnforce
	var runs []gen.OpenrailsDestructiveRun
	ctx := merchant.WithID(cmd.Context(), mid)
	if err := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		runs, e = database.Gen(ctx).ListDestructiveRuns(ctx, gen.ListDestructiveRunsParams{
			MerchantID: mid.UUID(), Kind: &kind, Lim: int32(limit),
		})
		return e
	}); err != nil {
		return err
	}

	if strings.EqualFold(strings.TrimSpace(format), "json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(runs)
	}
	if len(runs) == 0 {
		fmt.Println("no converge-enforce runs recorded for this merchant")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "RUN\tSTATUS\tPSP\tACTOR\tSTARTED\tEXPECTED\tAFFECTED")
	for i := range runs {
		r := &runs[i]
		psp := "-"
		if r.PspID != nil {
			psp = r.PspID.String()
		}
		expected := "-"
		if r.ExpectedRows != nil {
			expected = fmt.Sprintf("%d", *r.ExpectedRows)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Status, psp, r.Actor, r.StartedAt.UTC().Format("2006-01-02T15:04:05Z"), expected, string(r.Affected))
	}
	return w.Flush()
}

func runConvergeRollback(cmd *cobra.Command, merchantSlug, runIDStr, format string) error {
	runID, err := uuid.Parse(strings.TrimSpace(runIDStr))
	if err != nil {
		return fmt.Errorf("converge rollback: --run must be a destructive-run UUID (see `openrails converge list`): %w", err)
	}
	database, mid, err := convergeOpenDB(cmd, merchantSlug)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	var res reconcile.ConvergeRollbackResult
	ctx := merchant.WithID(cmd.Context(), mid)
	if err := database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		res, e = reconcile.RollbackConvergeEnforceRun(ctx, database, runID, cliActor())
		return e
	}); err != nil {
		return err
	}

	if strings.EqualFold(strings.TrimSpace(format), "json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	fmt.Printf("reversed run %s\n", res.RunID)
	fmt.Printf("  subscriptions restored : %d\n", res.SubscriptionsRestored)
	fmt.Printf("  unfired intents superseded: %d\n", res.IntentsSuperseded)
	fmt.Printf("  entitlement windows closed by the run: %d (NOT restored — Converge recomputes them from the grant log)\n", res.EntitlementsCaptured)
	fmt.Printf("  proven source domains reset to unproven: %d\n", res.ProvenDomainsReset)
	if res.EnforcementDisarmed {
		fmt.Println("  enforcement disarmed: the next pull for this merchant runs ADVISORY until an operator re-arms it")
	}
	for _, d := range res.IntentsIrreversible {
		fmt.Printf("  IRREVERSIBLE  %s %s (%s): %s\n", d.IntentType, d.IntentID, d.Status, d.Consequence)
	}
	for _, d := range res.IntentsAmbiguous {
		fmt.Printf("  AMBIGUOUS     %s %s (%s): %s\n", d.IntentType, d.IntentID, d.Status, d.Consequence)
	}
	if res.Complete() {
		fmt.Println("no provider write escaped: this reversal is complete on the local side.")
	} else {
		fmt.Printf("%d provider write(s) reached the rail before this reverse did. They are NOT undone and no automatic\n"+
			"re-write will be attempted — resubscribe / card re-entry is operator and customer work.\n",
			len(res.IntentsIrreversible)+len(res.IntentsAmbiguous))
	}
	fmt.Printf("local state is now BEFORE the run while the provider is at now — a rollback is not a complete operation.\n"+
		"Finish it:  openrails pull-provider --merchant %s     (advisory; review findings, then re-arm and re-run with mutation flags)\n",
		strings.TrimSpace(merchantSlug))
	return nil
}
