package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/history"
)

// newBookCmd arms the billing book in this database. A copy of the book (a
// dump restored elsewhere, another schema) is readonly until it is armed.
func newBookCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "book", Short: "Show or arm the billing book in this database"}
	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Report whether this database is the one the billing book was armed in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			database, err := openCLIDB(cmd.Context())
			if err != nil {
				return err
			}
			defer database.Close()
			armed, err := controls.BookArmed(cmd.Context(), database.GenDirectory())
			if err != nil {
				return err
			}
			if armed {
				fmt.Fprintln(cmd.OutOrStdout(), "armed: this database is the billing book's live copy")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "readonly: this database is a copy of the billing book; arm it only once every other copy is stopped")
			}
			return nil
		},
	})
	var by string
	arm := &cobra.Command{
		Use:   "arm --by NAME",
		Short: "Record this database as the billing book's one live copy",
		Long: "Provider writes stay readonly in a copy of the billing book. Arm a copy only once every other copy using " +
			"the same provider credentials is stopped for good: two live copies bill the same customers twice.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			database, err := openCLIDB(cmd.Context())
			if err != nil {
				return err
			}
			defer database.Close()
			if err := controls.ArmBook(cmd.Context(), database.GenDirectory(), strings.TrimSpace(by), time.Now()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "armed by %s\n", strings.TrimSpace(by))
			return nil
		},
	}
	arm.Flags().StringVar(&by, "by", "", "who attests that every other copy is stopped (required)")
	cmd.AddCommand(arm)
	return cmd
}

// newMerchantCmd sets a merchant's controls, shows them, or purges the
// merchant.
func newMerchantCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "merchant", Short: "Show or set a merchant's controls, or purge the merchant"}
	readonly, limited, full := controls.ReadOnly, controls.Limited, controls.Full
	held, released, armed, disarmed := true, false, true, false
	cmd.AddCommand(newMerchantPurgeCmd(), newMerchantControlsCmd(),
		controlCmd("writes full|limited|readonly", "Set how much OpenRails writes to a merchant's providers",
			"full writes everything; limited holds what the system initiates (renewals, retries, proactive deletes) "+
				"while what a person asks for runs; readonly sends nothing. An exported merchant is readonly at its source "+
				"and a restored one at its destination: set writes full only on the copy that stays live, once the other is stopped.",
			map[string]controls.Change{"full": {Writes: &full}, "limited": {Writes: &limited}, "readonly": {Writes: &readonly}}),
		controlCmd("rebills hold|release", "Hold or release the charges OpenRails initiates for a merchant",
			"Held, renewals and rebills OpenRails charges, dunning retries and scheduled usage collection wait, "+
				"with access kept; released, they run at the next billing pass. A customer's own charges (a purchase, "+
				"an immediate plan change, paying now) never wait.",
			map[string]controls.Change{"hold": {RebillsHeld: &held}, "release": {RebillsHeld: &released}}),
		controlCmd("destructive arm|disarm", "Arm or disarm a merchant's destructive actions",
			"Disarmed (the default), the merchant's provider pulls run advisory, recording findings and changing nothing, "+
				"and no cancellation, revocation or provider delete runs. Arm once its first pull's findings are reviewed.",
			map[string]controls.Change{"arm": {DestructiveArmed: &armed}, "disarm": {DestructiveArmed: &disarmed}}),
	)
	return cmd
}

// controlCmd sets one control, to the value its argument names, for one
// merchant or every merchant, recording each change in the merchant's
// configuration history.
func controlCmd(use, short, long string, values map[string]controls.Change) *cobra.Command {
	var slug, by, reason string
	var all bool
	names := slices.Sorted(maps.Keys(values))
	c := &cobra.Command{
		Use: use + " (--merchant NAME | --all-merchants) --by NAME --reason TEXT", Short: short, Long: long,
		Args: cobra.ExactArgs(1), ValidArgs: names,
		RunE: func(cmd *cobra.Command, args []string) error {
			change, ok := values[args[0]]
			if !ok {
				return fmt.Errorf("%q: want %s", args[0], strings.Join(names, ", "))
			}
			by, reason := strings.TrimSpace(by), strings.TrimSpace(reason)
			if by == "" || reason == "" {
				return errors.New("--by and --reason are required")
			}
			if all == (strings.TrimSpace(slug) != "") {
				return errors.New("name one merchant with --merchant, or every merchant with --all-merchants")
			}
			ctx := history.WithSource(cmd.Context(), billing.LogSourceCLI)
			database, err := openCLIDB(ctx)
			if err != nil {
				return err
			}
			defer database.Close()
			rows, err := cliMerchantControls(cmd, database, slug, all)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, row := range rows {
				after, changed, err := controls.Apply(ctx, database, billing.MerchantID(row.MerchantID), change, by, reason, time.Now())
				if err != nil {
					return fmt.Errorf("merchant %s: %w", row.Slug, err)
				}
				state := "unchanged"
				if changed {
					state = "changed by " + by
				}
				fmt.Fprintf(out, "merchant %s: writes %s, rebills %s, destructive %s (%s)\n", row.Slug, after.Writes, rebillsWord(after.RebillsHeld), armedWord(after.DestructiveArmed), state)
			}
			return nil
		},
	}
	c.Flags().StringVar(&slug, "merchant", "", "merchant public name or id:<uuid>")
	c.Flags().BoolVar(&all, "all-merchants", false, "every merchant in the directory")
	c.Flags().StringVar(&by, "by", "", "who sets it (required)")
	c.Flags().StringVar(&reason, "reason", "", "why (required)")
	return c
}

// newMerchantControlsCmd shows merchants' controls.
func newMerchantControlsCmd() *cobra.Command {
	var slug string
	c := &cobra.Command{
		Use:   "controls [--merchant NAME]",
		Short: "Show every merchant's controls, or one merchant's",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			database, err := openCLIDB(cmd.Context())
			if err != nil {
				return err
			}
			defer database.Close()
			rows, err := cliMerchantControls(cmd, database, slug, strings.TrimSpace(slug) == "")
			if err != nil {
				return err
			}
			return printControls(cmd.OutOrStdout(), rows)
		},
	}
	c.Flags().StringVar(&slug, "merchant", "", "merchant public name or id:<uuid>; every merchant when omitted")
	return c
}

// cliMerchantControls is every merchant's controls, or the named one's.
func cliMerchantControls(cmd *cobra.Command, database *db.DB, slug string, all bool) ([]gen.ListMerchantControlsRow, error) {
	rows, err := database.GenDirectory().ListMerchantControls(cmd.Context())
	if err != nil || all {
		return rows, err
	}
	mid, err := resolveCLIMerchant(cmd.Context(), database, slug)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.MerchantID == mid.UUID() {
			return []gen.ListMerchantControlsRow{row}, nil
		}
	}
	return nil, fmt.Errorf("merchant %s is not in the directory", mid)
}

func printControls(w io.Writer, rows []gen.ListMerchantControlsRow) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MERCHANT\tWRITES\tREBILLS\tDESTRUCTIVE\tFIRST PULL\tCHANGED")
	for _, row := range rows {
		pulled, changed := "-", "-"
		if row.FirstPullCompletedAt != nil {
			pulled = row.FirstPullCompletedAt.UTC().Format(time.RFC3339)
		}
		if row.ChangedAt != nil && row.ChangedBy != nil && row.Reason != nil {
			changed = fmt.Sprintf("%s by %s: %s", row.ChangedAt.UTC().Format(time.RFC3339), *row.ChangedBy, *row.Reason)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Slug, row.Writes, rebillsWord(row.RebillsHeld), armedWord(row.DestructiveArmed), pulled, changed)
	}
	return tw.Flush()
}

func rebillsWord(held bool) string {
	if held {
		return "held"
	}
	return "released"
}

func armedWord(armed bool) string {
	if armed {
		return "armed"
	}
	return "disarmed"
}
