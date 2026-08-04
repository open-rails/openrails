package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newConvergeCmd wires the or#859 inspection surface for converge-enforce, the
// sibling of `openrails prune list`:
//
//	openrails converge list --merchant <slug>
//
// A prune destroys ROWS and reverses by clearing their tombstones. An enforce
// pass destroys row VALUES — an empty NMI roster cancelling 40/40
// subscriptions overwrote status, ended_at, the grace/retry schedule and the
// period bounds, and queued deferred vault deletes behind them. Those are
// different reversals over ONE run ledger, and both are performed by
// `openrails undo-run`, which dispatches on the run's recorded kind.
func newConvergeCmd() *cobra.Command {
	var merchantSlug, format string
	var limit int

	cmd := &cobra.Command{
		Use:   "converge",
		Short: "Inspect enforcing provider-pull (converge-enforce) runs (or#859); reverse them with `openrails undo-run`",
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

	cmd.AddCommand(listCmd)
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
