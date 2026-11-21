package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/writeposture"
	"github.com/spf13/cobra"
)

func newPSPWriteModeCmd() *cobra.Command {
	var merchant, psp, mode string
	cmd := &cobra.Command{
		Use:   "psp-write-mode",
		Short: "Read or explicitly set the write mode of one payment provider account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			pspID, err := billing.ParsePSPID(strings.TrimSpace(psp))
			if err != nil {
				return err
			}
			return withCLIDB(cmd, func(ctx context.Context, database *db.DB) error {
				mid, err := resolveCLIMerchant(ctx, database, merchant)
				if err != nil {
					return err
				}
				q := database.Gen(ctx)
				if cmd.Flags().Changed("mode") {
					chosen, ok := writeposture.ParseMode(strings.TrimSpace(mode))
					if !ok {
						return fmt.Errorf("--mode must be readonly, limited or full")
					}
					count, err := q.SetPSPWriteMode(ctx, gen.SetPSPWriteModeParams{MerchantID: mid.UUID(), PspID: pspID.UUID(), WriteMode: chosen.String()})
					if err != nil {
						return err
					}
					if count == 0 {
						return fmt.Errorf("payment provider account %s was not found for merchant %s", pspID, mid)
					}
				}
				account, err := q.GetPSP(ctx, gen.GetPSPParams{MerchantID: mid.UUID(), ID: pspID.UUID()})
				if err != nil {
					return err
				}
				cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
				posture := (writeposture.View{Config: cfg, DB: database}).Provider(ctx, mid.UUID(), pspID.UUID())
				return printJSON(cmd.OutOrStdout(), map[string]string{
					"merchant_id": mid.String(), "psp_id": pspID.String(), "write_mode": account.WriteMode,
					"effective_mode": posture.Mode.String(), "reason": posture.Reason,
				})
			})
		},
	}
	cmd.Flags().StringVar(&merchant, "merchant", "", "Merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&psp, "psp", "", "Payment provider account ID (required)")
	cmd.Flags().StringVar(&mode, "mode", "", "readonly, limited or full. Omit to inspect the current mode.")
	_ = cmd.MarkFlagRequired("merchant")
	_ = cmd.MarkFlagRequired("psp")
	return cmd
}
