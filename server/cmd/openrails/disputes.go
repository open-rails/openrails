package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/server/internal/hostconfig"
)

func newDisputesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "disputes", Short: "Resolve unmatched provider disputes"}
	var merchantSlug, dispute, payment, actor, reason string
	attach := &cobra.Command{
		Use: "attach", Short: "Book an unmatched NMI chargeback against the charge it disputes", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			disputeID, err := billing.ParseDisputeID(strings.TrimSpace(dispute))
			if err != nil {
				return fmt.Errorf("invalid --dispute: %w", err)
			}
			paymentID, err := billing.ParsePaymentID(strings.TrimSpace(payment))
			if err != nil {
				return fmt.Errorf("invalid --payment: %w", err)
			}
			return runDisputesAttach(c.Context(), hostconfig.Engine(c.Context()), merchantSlug, disputeID.UUID(), paymentID.UUID(), actor, reason)
		},
	}
	attach.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	attach.Flags().StringVar(&dispute, "dispute", "", "Unmatched dispute id (dsp_..., required)")
	attach.Flags().StringVar(&payment, "payment", "", "Disputed charge id (pay_..., required)")
	attach.Flags().StringVar(&actor, "actor", "", "Operator recorded with the resolution (required)")
	attach.Flags().StringVar(&reason, "reason", "", "Evidence identifying the charge (required)")
	cmd.AddCommand(attach)
	return cmd
}

func runDisputesAttach(ctx context.Context, cfg *billing.Config, merchantSlug string, disputeID, paymentID uuid.UUID, actor, reason string) error {
	if cfg == nil {
		return fmt.Errorf("config not loaded")
	}
	if strings.TrimSpace(actor) == "" || actor == "unknown" || strings.TrimSpace(reason) == "" {
		return fmt.Errorf("--actor and --reason are required")
	}
	pool, vault, err := cliDeps(ctx)
	if err != nil {
		return err
	}
	application, err := app.BootstrapWithOptions(ctx, cfg, &app.BootstrapOptions{Logger: slog.Default(), PGXPool: pool, Vault: vault})
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() { _ = application.Close(context.Background()) }()
	if err := application.Runtime.EnsureMerchantsService(ctx); err != nil {
		return fmt.Errorf("arm merchant credentials: %w", err)
	}
	rt := application.Runtime
	mid, err := resolveCLIMerchant(ctx, rt.DB, merchantSlug)
	if err != nil {
		return err
	}
	ctx = merchant.WithID(ctx, mid)
	return rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		if err := rt.WebhookDispatcher.AttachNMIChargeback(ctx, disputeID, paymentID, actor, reason); err != nil {
			return err
		}
		row, err := rt.DB.Gen(ctx).GetDispute(ctx, gen.GetDisputeParams{MerchantID: mid.UUID(), ID: disputeID})
		if err != nil {
			return err
		}
		out := map[string]any{"dispute": billing.DisputeID(disputeID), "payment": billing.PaymentID(paymentID), "reversal": nil}
		if row.ReversalPaymentID != nil {
			out["reversal"] = billing.PaymentID(*row.ReversalPaymentID)
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	})
}
