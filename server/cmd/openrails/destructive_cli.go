package main

import (
	"context"
	"fmt"
	"github.com/open-rails/openrails/billing"
	"github.com/spf13/cobra"
	"strings"
)

func newDestructiveHoldsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "destructive-holds", Short: "Inspect and explicitly clear persistent destructive-action incidents"}
	var limit int
	var after string
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var id billing.DestructiveHoldID
		if after != "" {
			var err error
			id, err = billing.ParseDestructiveHoldID(after)
			if err != nil {
				return err
			}
		}
		return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
			items, err := srv.ListOpenDestructiveHolds(ctx, limit, id)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), items)
		})
	}}
	list.Flags().IntVar(&limit, "limit", 100, "maximum incidents, 1 to 100")
	list.Flags().StringVar(&after, "after", "", "last incident ID from the preceding page")
	var issuer, actor, reason string
	clear := &cobra.Command{Use: "clear <merchant> <hold-id>", Short: "Clear one incident after review, retaining pending action reservations", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(issuer) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(reason) == "" {
			return fmt.Errorf("operator-issuer, operator and reason are required")
		}
		id, err := billing.ParseDestructiveHoldID(args[1])
		if err != nil {
			return err
		}
		return withServer(cmd, func(ctx context.Context, srv operatorServer) error {
			mid, err := merchantArg(ctx, srv, args[0])
			if err != nil {
				return err
			}
			out, err := srv.ClearDestructiveHold(ctx, mid, id, billing.DestructiveOperator{Issuer: issuer, ID: actor}, reason)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		})
	}}
	clear.Flags().StringVar(&issuer, "operator-issuer", "", "verified operator's identity issuer")
	clear.Flags().StringVar(&actor, "operator", "", "verified human operator's subject ID")
	clear.Flags().StringVar(&reason, "reason", "", "reviewed reason for clearing this incident")
	cmd.AddCommand(list, clear)
	return cmd
}
