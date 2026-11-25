package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	"github.com/spf13/cobra"
)

func newDestructiveHoldsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "destructive-holds", Short: "Inspect and explicitly clear persistent destructive-action incidents", Long: `A destructive-action incident opens when staff exceed
destructive_action_limits. It holds new provider requests for refunds,
cancellations, removals, revocations, write-offs and substantive catalog or
configuration changes until a person clears it: waiting, restarting, rotating
a key or resolving a finding does not. Reads and the completion of effects
already started continue.

Review before clearing: find the actor in the findings queue, revoke its
credential where appropriate, and reconcile pending effects
("openrails intents"). Clearing ends one episode and records the operator and
reason; pending or uncertain actions stay reserved and a further breach opens
a new incident. Over HTTP the clear takes merchant configuration authority
plus root:admin-rate-limits:unlock from a self-acting person other than the
triggering actor.`}
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
			gate := destructive.NewAdmissions(database, nil, nil, billing.DestructiveActionLimitsConfig{})
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
			cfg := hostconfig.Engine(ctx)
			runtime := &app.Runtime{DB: database, Config: cfg, Log: slog.Default()}
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
