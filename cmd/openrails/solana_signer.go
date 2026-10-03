package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchantsecrets"
)

// newSolanaSignerCmd groups the Vault Transit signer operator commands.
func newSolanaSignerCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "solana-signer", Short: "Vault Transit Solana signer identity"}
	cmd.AddCommand(newSolanaSignerApproveCmd())
	return cmd
}

// newSolanaSignerApproveCmd approves the identity a changed Transit key now
// reports. Until then the Solana rail refuses and the signer probe fails.
func newSolanaSignerApproveCmd() *cobra.Command {
	var merchantSlug, key, manifestPath string
	cmd := &cobra.Command{
		Use:   "approve",
		Short: "Approve the new Solana identity a changed Vault Transit key reports (verify the public key in the ERROR log first)",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, _ := c.Context().Value(config.ConfigContextKey).(*config.Config)
			if cfg == nil {
				return fmt.Errorf("config not loaded")
			}
			if strings.TrimSpace(key) == "" {
				return fmt.Errorf("--key is required")
			}
			return runSolanaSignerApprove(c.Context(), cfg, merchantSlug, strings.TrimSpace(key), manifestPath)
		},
	}
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant public name or id:<uuid> (required)")
	cmd.Flags().StringVar(&key, "key", "", "Vault Transit key name (signer.key) (required)")
	cmd.Flags().StringVar(&manifestPath, "merchant-manifest", "", "Merchant manifest the server boots with (default: the conventional path when present)")
	return cmd
}

func runSolanaSignerApprove(ctx context.Context, cfg *config.Config, merchantSlug, key, manifestPath string) error {
	client, graph, err := openEngine(ctx, cfg, openrails.Deps{}, true)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close(context.Background()) }()
	if err := graph.Runtime.MerchantSecretBackend.Await(ctx, merchantsecrets.AwaitTimeout); err != nil {
		return err
	}
	if err := serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, manifestPath, ""); err != nil {
		return err
	}
	mid, err := resolveCLIMerchant(ctx, graph.Runtime.DB, merchantSlug)
	if err != nil {
		return err
	}
	if err := graph.Runtime.ApproveSolanaSigner(ctx, mid, key); err != nil {
		return err
	}
	fmt.Printf("approved the Solana identity Vault Transit key %q now reports for merchant %s\n", key, mid)
	return nil
}
