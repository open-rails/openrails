package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/spf13/cobra"
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
		return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
			gate := destructive.NewAdmissions(database, nil, nil)
			var cursor *uuid.UUID
			if !id.IsZero() {
				value := id.UUID()
				cursor = &value
			}
			addresses, err := gate.ListOpenHoldsDirectory(ctx, limit, cursor)
			if err != nil {
				return err
			}
			items := make([]billing.DestructiveHoldReview, 0, len(addresses))
			for _, address := range addresses {
				hold, err := gate.GetHold(ctx, address.MerchantID, address.ID)
				if err != nil {
					return err
				}
				items = append(items, hold.Review())
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
		return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
			mid, err := merchantArg(ctx, database, args[0])
			if err != nil {
				return err
			}
			cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
			runtime := &app.Runtime{DB: database, Config: cfg}
			if err := runtime.WireOneOffDestructiveAdmissions(ctx); err != nil {
				return err
			}
			defer runtime.Close(context.WithoutCancel(ctx))
			hold, err := runtime.DestructiveAdmissions.Clear(ctx, mid.UUID(), id.UUID(), destructive.Actor{Issuer: issuer, ID: actor, Credential: "trusted-operator"}, reason)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), hold.View())
		})
	}}
	clear.Flags().StringVar(&issuer, "operator-issuer", "", "verified operator's identity issuer")
	clear.Flags().StringVar(&actor, "operator", "", "verified human operator's subject ID")
	clear.Flags().StringVar(&reason, "reason", "", "reviewed reason for clearing this incident")
	cmd.AddCommand(list, clear)
	return cmd
}
