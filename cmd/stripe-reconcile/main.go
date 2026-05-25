package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/joho/godotenv"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/modules/reconcile"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func main() {
	cmd := &cobra.Command{
		Use:   "stripe-reconcile",
		Short: "Reconcile/backfill local subscriptions, payments, and cards against Stripe",
		Long: "Pages through Stripe subscriptions and charges, maps each to an OpenRails user/price, " +
			"and mirrors remote state into the local DB: missing memberships, missing payments, " +
			"per-payment card snapshots, and the current payment method per subscription. " +
			"Dry-run by default; pass --apply to write. Never charges anything; safe to re-run.",
		RunE: run,
	}
	cmd.CompletionOptions.DisableDefaultCmd = false

	cmd.Flags().String("config", "./config.yaml", "Path to config file")
	cmd.Flags().Bool("apply", false, "Write changes (memberships/payments/cards); default is dry-run")
	cmd.Flags().Int("max-pages", 0, "Max pages to fetch from Stripe (0 = unlimited)")
	cmd.Flags().Int("page-size", 100, "Page size for Stripe list calls (max 100)")
	cmd.Flags().Bool("skip-payments", false, "Reconcile subscriptions only (skip the charges/payments/cards pass)")

	if err := loadDotEnv(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	viper.AutomaticEnv()
	_ = viper.BindPFlags(cmd.Flags())

	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func loadDotEnv() error {
	if err := godotenv.Load(); err != nil {
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return err
		}
	}
	return nil
}

func run(cmd *cobra.Command, _ []string) error {
	configPath := viper.GetString("config")
	opts := reconcile.Options{
		Apply:        viper.GetBool("apply"),
		MaxPages:     viper.GetInt("max-pages"),
		PageSize:     viper.GetInt("page-size"),
		SkipPayments: viper.GetBool("skip-payments"),
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("config validation failed: %w", err)
	}

	application, err := bootstrap.NewApp(cfg, nil)
	if err != nil {
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	defer application.Close(context.Background())

	subLister, err := subscriptions.NewStripeSubscriptionLister(cfg)
	if err != nil {
		return fmt.Errorf("stripe subscription lister init failed: %w", err)
	}

	var chargeLister subscriptions.StripeChargeLister
	if !opts.SkipPayments {
		cl, err := subscriptions.NewStripeChargeLister(cfg)
		if err != nil {
			return fmt.Errorf("stripe charge lister init failed: %w", err)
		}
		chargeLister = cl
	}

	_, err = reconcile.Run(context.Background(), application.Runtime, subLister, chargeLister, opts)
	return err
}
