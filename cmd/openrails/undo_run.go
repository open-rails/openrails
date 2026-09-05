package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/pkg/embedded"
)

// newUndoRunCmd wires or#859 §5.2's single undo verb over the whole
// destructive-run ledger:
//
//	openrails undo-run --merchant <slug> --run <id>                        # plan
//	openrails undo-run --merchant <slug> --run <id> --apply --expect-rows N
//
// One verb, not one per kind. `--prune` destroys rows and reverses by clearing
// tombstones; a converge-enforce pass destroys row VALUES and reverses from
// captured before-images — but an operator holding a run id in the middle of an
// incident should not have to know which, and must not be able to learn the
// difference by typing the wrong verb and being told a reversal succeeded when
// it restored nothing. The kind is read from the ledger and dispatched on, and a
// kind with no undo is refused by name with what to reach for instead.
func newUndoRunCmd() *cobra.Command {
	var merchantSlug, runID, format string
	var apply bool
	var expectRows int64

	cmd := &cobra.Command{
		Use:   "undo-run",
		Short: "Plan or apply the reversal of one destructive run (prune or converge-enforce)",
		Long: "Reverse one destructive run by id, whatever kind it is.\n\n" +
			"DRY RUN BY DEFAULT. With no --apply the command prints what it would restore, the provider writes it\n" +
			"would supersede, the ones that already fired and cannot be undone, and the NULL-psp rows no PSP-scoped\n" +
			"predicate can reach. Applying additionally requires --expect-rows to match that plan: an undo IS a mass\n" +
			"mutation of the live book, at the worst possible moment to be wrong.\n\n" +
			"Scope is a property of the run, not a flag. The ledger row carries the merchant and — when the pass was\n" +
			"account-bound — the PSP, and every restore predicate is keyed on the run id inside a merchant-scoped\n" +
			"connection. There is no widening knob.\n\n" +
			"What it will NOT do, on purpose:\n" +
			"  * restore an entitlement — the windows a converge run closed are INVALIDATED and Converge rebuilds\n" +
			"    them from the append-only grant log. A restored effect can silently disagree with its grant\n" +
			"  * touch the ledger, grants, status transitions, or the intent/webhook/findings logs\n" +
			"  * pretend a provider write that already fired was undone; those are reported as irreversible divergence\n" +
			"  * reverse a kind whose damage no local undo can reach (a merchant purge) — it says so instead\n\n" +
			"A rollback is not a complete operation. `rollback -> pull -> converge` is. The re-derivation runs inside\n" +
			"this command; the provider pull is the operator's next step and runs ADVISORY until enforcement is re-armed.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			if apply && !c.Flags().Changed("expect-rows") {
				return fmt.Errorf("undo-run --apply requires --expect-rows: run without --apply first and confirm the row count it prints")
			}
			cfg, _ := c.Context().Value(config.ConfigContextKey).(*config.Config)
			mid, err := resolveConfiguredCLIMerchant(c.Context(), cfg, merchantSlug)
			if err != nil {
				return err
			}
			return embedded.UndoRun(c.Context(), embedded.UndoRunOptions{
				Config: cfg, MerchantID: mid, RunID: runID, Actor: cliActor(),
				Apply: apply, ExpectRows: expectRows, Format: format, Out: os.Stdout,
			})
		},
	}
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&runID, "run", "", "Destructive run id to reverse (required; see `openrails prune list` / `openrails converge list`)")
	cmd.Flags().BoolVar(&apply, "apply", false, "Actually perform the reversal (default: plan only)")
	cmd.Flags().Int64Var(&expectRows, "expect-rows", 0, "Typed confirmation: the number of rows the plan says would be restored. Required with --apply")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	return cmd
}
