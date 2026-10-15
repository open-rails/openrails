package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/modules/payment"
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
			return runSolanaPayResolve(c.Context(), cfg, merchantSlug, strings.TrimSpace(signature), strings.TrimSpace(resolution), c.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&signature, "signature", "", "Transaction signature of the review receipt (required)")
	cmd.Flags().StringVar(&resolution, "resolution", "", "How the money was settled, e.g. the refund transaction signature (required)")
	return cmd
}

func runSolanaPayResolve(ctx context.Context, cfg *config.Config, merchantSlug, signature, resolution string, out io.Writer) error {
	database, err := openCLIDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	mid, err := resolveCLIMerchant(ctx, database, merchantSlug)
	if err != nil {
		return err
	}
	// Resolving a recorded review only needs the attempt service's ledger; it
	// never quotes, charges or talks to a chain/provider.
	attempts := payment.NewOrderAttemptService(database, nil, nil, nil, nil, nil, nil, nil, nil, cfg, nil)
	graph := &app.App{Config: cfg, Runtime: &app.Runtime{DB: database, Config: cfg, OrderAttemptService: attempts}}
	if err := hosttools.ResolveSolanaPayReview(ctx, graph, mid, signature, resolution); err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "Resolved Solana Pay review %s for merchant %s.\n", signature, mid)
	return err
}
