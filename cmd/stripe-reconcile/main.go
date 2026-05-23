package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const stripeProcessorName = "stripe"

func main() {
	cmd := &cobra.Command{
		Use:   "stripe-reconcile",
		Short: "Reconcile/backfill local subscriptions against Stripe",
		Long: "Pages through Stripe subscriptions, maps each to an OpenRails user and price, " +
			"and diffs the remote active set against local active subscriptions. " +
			"Dry-run by default; pass --apply to create local memberships for remote-only subs.",
		RunE: run,
	}
	cmd.CompletionOptions.DisableDefaultCmd = false

	cmd.Flags().String("config", "./config.yaml", "Path to config file")
	cmd.Flags().Bool("apply", false, "Create memberships for remote-only Stripe subscriptions (default is dry-run)")
	cmd.Flags().Int("max-pages", 0, "Max pages to fetch from Stripe (0 = unlimited)")
	cmd.Flags().Int("page-size", 100, "Page size for the Stripe subscriptions list (max 100)")

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

type reconcileOptions struct {
	apply    bool
	maxPages int
	pageSize int
}

func run(cmd *cobra.Command, _ []string) error {
	configPath := viper.GetString("config")
	opts := reconcileOptions{
		apply:    viper.GetBool("apply"),
		maxPages: viper.GetInt("max-pages"),
		pageSize: viper.GetInt("page-size"),
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

	lister, err := subscriptions.NewStripeSubscriptionLister(cfg)
	if err != nil {
		return fmt.Errorf("stripe lister init failed: %w", err)
	}

	return reconcileStripe(context.Background(), application, lister, opts)
}

// priceResolver is the subset of catalog.PriceService the mapper needs. Keeping
// it as an interface lets the mapping logic be unit tested without a database.
type priceResolver interface {
	GetByID(ctx context.Context, id uuid.UUID) (*models.Price, error)
	GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error)
}

// mappedSubscription is a remote Stripe subscription that resolved to a local
// user and price.
type mappedSubscription struct {
	Remote  subscriptions.StripeRemoteSubscription
	UserID  string
	PriceID uuid.UUID
}

// mapRemoteSubscription resolves a remote Stripe subscription to an OpenRails
// user and price. The user comes from metadata["user_id"]; the price resolves
// from metadata["internal_price_id"] (a local price UUID), falling back to the
// line-item Stripe price id. A non-nil reason means the sub could not be mapped
// and should be skipped/reported.
func mapRemoteSubscription(ctx context.Context, prices priceResolver, remote subscriptions.StripeRemoteSubscription) (*mappedSubscription, string) {
	userID := normalize.FirstNonEmpty(remote.Metadata["user_id"], remote.Metadata["userId"], remote.Metadata["uid"])
	if userID == "" {
		return nil, "missing user_id metadata"
	}

	if rawID := normalize.FirstNonEmpty(remote.Metadata["internal_price_id"], remote.Metadata["price_id"]); rawID != "" {
		priceID, parseErr := uuid.Parse(rawID)
		if parseErr == nil {
			price, err := prices.GetByID(ctx, priceID)
			if err == nil && price != nil {
				return &mappedSubscription{Remote: remote, UserID: userID, PriceID: price.ID}, ""
			}
		}
	}

	if stripePriceID := strings.TrimSpace(remote.StripePriceID); stripePriceID != "" {
		price, err := prices.GetByStripePriceID(ctx, stripePriceID)
		if err == nil && price != nil {
			return &mappedSubscription{Remote: remote, UserID: userID, PriceID: price.ID}, ""
		}
	}

	return nil, "no internal_price_id or mapped stripe price id"
}

// diffResult holds the keyed diff between remote and local subscription id sets.
type diffResult struct {
	RemoteOnly []string
	LocalOnly  []string
	Matched    int
}

// diffSubscriptions compares the set of remote and local subscription ids,
// mirroring subscription-sync's diffKeys behavior.
func diffSubscriptions(remoteIDs, localIDs map[string]struct{}) diffResult {
	result := diffResult{
		RemoteOnly: diffKeys(remoteIDs, localIDs),
		LocalOnly:  diffKeys(localIDs, remoteIDs),
	}
	for id := range remoteIDs {
		if _, ok := localIDs[id]; ok {
			result.Matched++
		}
	}
	return result
}

func diffKeys(a, b map[string]struct{}) []string {
	out := make([]string, 0)
	for k := range a {
		if _, ok := b[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func reconcileStripe(ctx context.Context, application *app.App, lister subscriptions.StripeSubscriptionLister, opts reconcileOptions) error {
	fmt.Printf("\n== %s ==\n", stripeProcessorName)

	remoteSubs, err := lister.ListSubscriptions(ctx, opts.pageSize, opts.maxPages)
	if err != nil {
		return fmt.Errorf("fetch stripe subscriptions failed: %w", err)
	}

	priceService := application.Runtime.PriceService
	if priceService == nil {
		priceService = catalog.NewPriceService(application.Runtime.DB)
	}

	// Build the remote active set, mapping each to a user/price. Unmappable or
	// inactive subs are reported and excluded from the diff.
	remoteIDs := make(map[string]struct{})
	mapped := make(map[string]*mappedSubscription)
	remoteActiveCount := 0
	skipped := make([]string, 0)

	for _, remote := range remoteSubs {
		if remote.ID == "" {
			continue
		}
		if !subscriptions.StripeSubscriptionStatusActive(remote.Status) {
			continue
		}
		remoteActiveCount++
		if remote.CancelAtPeriodEnd {
			skipped = append(skipped, fmt.Sprintf("%s (cancel_at_period_end backfill unsupported)", remote.ID))
			continue
		}

		m, reason := mapRemoteSubscription(ctx, priceService, remote)
		if reason != "" {
			skipped = append(skipped, fmt.Sprintf("%s (%s)", remote.ID, reason))
			continue
		}
		remoteIDs[remote.ID] = struct{}{}
		mapped[remote.ID] = m
	}

	subService := subscriptions.NewSubscriptionService(application.Runtime.DB, nil, nil, nil, nil, nil)
	localSubs, err := subService.GetActiveSubscriptionsByProcessor(ctx, stripeProcessorName)
	if err != nil {
		return fmt.Errorf("fetch local subscriptions failed: %w", err)
	}

	localIDs := make(map[string]struct{}, len(localSubs))
	for _, sub := range localSubs {
		id := strings.TrimSpace(sub.ProcessorSubscriptionID)
		if id != "" {
			localIDs[id] = struct{}{}
		}
	}

	diff := diffSubscriptions(remoteIDs, localIDs)

	fmt.Printf("remote subscriptions fetched: %d (active=%d, mappable=%d, skipped=%d)\n", len(remoteSubs), remoteActiveCount, len(remoteIDs), len(skipped))
	fmt.Printf("local active subscriptions: %d (unique ids=%d)\n", len(localSubs), len(localIDs))
	fmt.Printf("matched subscriptions: %d\n", diff.Matched)

	if len(skipped) == 0 {
		fmt.Println("skipped (unmappable) subscriptions: none")
	} else {
		fmt.Printf("skipped (unmappable) subscriptions (%d):\n", len(skipped))
		for _, entry := range skipped {
			fmt.Printf("  %s\n", entry)
		}
	}

	if len(diff.RemoteOnly) == 0 {
		fmt.Println("remote-only subscriptions: none")
	} else {
		fmt.Printf("remote-only subscriptions (%d):\n", len(diff.RemoteOnly))
		for _, id := range diff.RemoteOnly {
			fmt.Printf("  %s\n", id)
		}
	}

	if len(diff.LocalOnly) == 0 {
		fmt.Println("local-only subscriptions: none")
	} else {
		fmt.Printf("local-only subscriptions (%d) — flagged only, not cancelled:\n", len(diff.LocalOnly))
		for _, id := range diff.LocalOnly {
			fmt.Printf("  %s\n", id)
		}
	}

	if !opts.apply {
		if len(diff.RemoteOnly) > 0 {
			fmt.Printf("\ndry-run: would create %d local membership(s); pass --apply to write\n", len(diff.RemoteOnly))
		}
		return nil
	}

	if len(diff.RemoteOnly) == 0 {
		return nil
	}

	// Safety guard: refuse a large/destructive apply when the remote report
	// returned zero subscriptions while local active subs exist (mirrors the
	// NMI guard in subscription-sync). A zero-length remote report most likely
	// indicates an auth/API problem rather than a genuinely empty account.
	if len(remoteSubs) == 0 && len(localIDs) > 0 {
		return fmt.Errorf("refusing to apply stripe reconciliation: remote report returned zero subscriptions while %d local active subscriptions exist", len(localIDs))
	}

	if application.Runtime.SubscriptionLifecycleService == nil {
		return fmt.Errorf("subscription lifecycle service unavailable; cannot apply changes")
	}

	created := 0
	for _, id := range diff.RemoteOnly {
		m, ok := mapped[id]
		if !ok {
			fmt.Fprintf(os.Stderr, "skip %s: missing mapping\n", id)
			continue
		}
		subID := id
		_, err := application.Runtime.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
			UserID:                  m.UserID,
			PriceID:                 m.PriceID,
			Processor:               models.ProcessorStripe,
			ProcessorSubscriptionID: &subID,
			CurrentPeriodStartsAt:   zeroTimeNil(m.Remote.CurrentPeriodStart),
			CurrentPeriodEndsAt:     zeroTimeNil(m.Remote.CurrentPeriodEnd),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to create membership for %s: %v\n", id, err)
			continue
		}
		created++
		fmt.Printf("created local membership for remote subscription %s (user=%s price=%s)\n", id, m.UserID, m.PriceID)
	}
	fmt.Printf("created %d local membership(s)\n", created)

	return nil
}

func zeroTimeNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
