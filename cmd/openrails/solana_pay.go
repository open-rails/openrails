package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
)

// newSolanaPayCmd groups the Solana Pay operator commands.
func newSolanaPayCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "solana-pay", Short: "Solana Pay receipts"}
	cmd.AddCommand(newSolanaPayResolveCmd())
	return cmd
}

// newSolanaPayResolveCmd closes a review receipt once its money was refunded
// or otherwise settled.
func newSolanaPayResolveCmd() *cobra.Command {
	var merchantSlug, signature, resolution string
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Close a Solana Pay review receipt after refunding or settling its money",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, _ := c.Context().Value(config.ConfigContextKey).(*config.Config)
			if cfg == nil {
				return fmt.Errorf("config not loaded")
			}
			if strings.TrimSpace(signature) == "" || strings.TrimSpace(resolution) == "" {
				return fmt.Errorf("--signature and --resolution are required")
			}
			return runSolanaPayResolve(c.Context(), cfg, merchantSlug, strings.TrimSpace(signature), strings.TrimSpace(resolution))
		},
	}
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&signature, "signature", "", "Transaction signature of the review receipt (required)")
	cmd.Flags().StringVar(&resolution, "resolution", "", "How the money was settled, e.g. the refund transaction signature (required)")
	return cmd
}

func runSolanaPayResolve(ctx context.Context, cfg *config.Config, merchantSlug, signature, resolution string) error {
	client, graph, err := openEngine(ctx, cfg, openrails.Deps{}, false)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close(context.Background()) }()
	mid, err := resolveCLIMerchant(ctx, graph.Runtime.DB, merchantSlug)
	if err != nil {
		return err
	}
	if err := hosttools.ResolveSolanaPayReview(ctx, graph, mid, signature, resolution); err != nil {
		return err
	}
	fmt.Printf("resolved Solana Pay review %s for merchant %s\n", signature, mid)
	return nil
}
