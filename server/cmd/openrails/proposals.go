package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

// newProposalsCmd is the pull.* plane of the findings queue from the CLI:
//
//	openrails proposals list    --merchant M [--psp KEY] [--kind K] [--source S]
//	openrails proposals show    --merchant M <id>
//	openrails proposals approve --merchant M <id>… --notes N
//	openrails proposals approve --merchant M --psp KEY --kind K --expect N --notes N
//	openrails proposals dismiss --merchant M <id>… --notes N
//	openrails proposals runs    --merchant M
//
// A filtered approve prints the set and applies it only when --expect equals
// its size; at most 100 a call. Each approve call is one approval run, which
// `openrails undo-run --run <id>` reverses.
func newProposalsCmd() *cobra.Command {
	var merchantSlug, format, psp, kind, source, notes string
	var expect, limit int

	options := func(c *cobra.Command) (hosttools.ProposalsOptions, error) {
		cfg, _ := c.Context().Value(config.ConfigContextKey).(*config.Config)
		mid, err := resolveConfiguredCLIMerchant(c.Context(), cfg, merchantSlug)
		if err != nil {
			return hosttools.ProposalsOptions{}, err
		}
		return hosttools.ProposalsOptions{Config: cfg, MerchantID: mid, Actor: cliActor(), Format: format, Out: os.Stdout}, nil
	}
	set := func(c *cobra.Command, ids []string) (hosttools.ProposalSet, error) {
		s := hosttools.ProposalSet{IDs: ids, PSP: psp, Kind: kind, Source: source, Expect: expect}
		if len(ids) > 0 && (psp != "" || kind != "" || source != "") {
			return s, fmt.Errorf("name proposal ids or a filtered set, not both")
		}
		if len(ids) == 0 && !c.Flags().Changed("expect") {
			return s, fmt.Errorf("a filtered set needs --expect: run `proposals list` with the same filters and type back the count")
		}
		return s, nil
	}

	cmd := &cobra.Command{
		Use:   "proposals",
		Short: "List, approve and dismiss the changes provider reads propose",
		Long: "A provider read never applies a change on its own: each difference it finds is a proposal (a pull.* finding)\n" +
			"that a person approves or dismisses. Approving applies the change through the lifecycle decider when its basis\n" +
			"still holds; the kill switch holds changes that end access or write to a provider, a readonly posture changes that\n" +
			"move money or write to a provider.",
	}
	persistent := cmd.PersistentFlags()
	persistent.StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	persistent.StringVar(&format, "format", "table", "Output format: table, json")

	filters := func(c *cobra.Command) {
		c.Flags().StringVar(&psp, "psp", "", "Only proposals raised by this PSP key")
		c.Flags().StringVar(&kind, "kind", "", "Only proposals of this change kind (record_charge, cancel_subscription, …)")
		c.Flags().StringVar(&source, "source", "", "Only proposals from this source: pull, verification")
	}

	list := &cobra.Command{
		Use: "list", Short: "List open proposals, most severe first", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			opts, err := options(c)
			if err != nil {
				return err
			}
			return hosttools.ProposalsList(c.Context(), opts, hosttools.ProposalSet{PSP: psp, Kind: kind, Source: source})
		},
	}
	filters(list)

	show := &cobra.Command{
		Use: "show <id>", Short: "Show one proposal and its change", Args: cobra.ExactArgs(1), SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			opts, err := options(c)
			if err != nil {
				return err
			}
			return hosttools.ProposalsShow(c.Context(), opts, args[0])
		},
	}

	resolveCmd := func(use, short string, run func(*cobra.Command, hosttools.ProposalsOptions, hosttools.ProposalSet) error) *cobra.Command {
		c := &cobra.Command{
			Use: use, Short: short, Args: cobra.MaximumNArgs(billing.MaxBatchItems), SilenceUsage: true,
			RunE: func(c *cobra.Command, args []string) error {
				opts, err := options(c)
				if err != nil {
					return err
				}
				s, err := set(c, args)
				if err != nil {
					return err
				}
				return run(c, opts, s)
			},
		}
		filters(c)
		c.Flags().IntVar(&expect, "expect", 0, "Typed confirmation of a filtered set's size")
		c.Flags().StringVar(&notes, "notes", "", "Why (required to dismiss)")
		return c
	}
	approve := resolveCmd("approve [<id>…]", "Approve proposals: each applies in its own transaction",
		func(c *cobra.Command, opts hosttools.ProposalsOptions, s hosttools.ProposalSet) error {
			return hosttools.ProposalsApprove(c.Context(), opts, s, notes)
		})
	dismiss := resolveCmd("dismiss [<id>…]", "Dismiss proposals without applying them",
		func(c *cobra.Command, opts hosttools.ProposalsOptions, s hosttools.ProposalSet) error {
			return hosttools.ProposalsDismiss(c.Context(), opts, s, notes)
		})

	runs := &cobra.Command{
		Use: "runs", Short: "List approval runs, newest first; reverse one with `openrails undo-run`", Args: cobra.NoArgs, SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			opts, err := options(c)
			if err != nil {
				return err
			}
			return hosttools.ProposalsRuns(c.Context(), opts, limit)
		},
	}
	runs.Flags().IntVar(&limit, "limit", 20, "Maximum runs to show")

	cmd.AddCommand(list, show, approve, dismiss, runs)
	return cmd
}
