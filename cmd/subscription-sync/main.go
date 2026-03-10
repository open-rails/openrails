package main

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type nmResponse struct {
	Subscriptions []nmSubscription `xml:"subscription"`
}

type nmSubscription struct {
	SubscriptionID string `xml:"subscription_id"`
}

func main() {
	cmd := &cobra.Command{
		Use:     "subscription-sync",
		Aliases: []string{"nmi-sync"},
		Short:   "Reconcile local subscriptions against processor reports",
		RunE:    run,
	}
	cmd.CompletionOptions.DisableDefaultCmd = false

	cmd.Flags().String("config", "./config.yaml", "Path to config file")
	cmd.Flags().String("processor", "mobius", "Processor name to reconcile (e.g., mobius, ccbill)")
	cmd.Flags().StringSlice("processors", nil, "Processor names to reconcile (repeatable or comma-separated)")
	cmd.Flags().Int("result-limit", 100, "Max results per page for NMI query.php")
	cmd.Flags().String("result-order", "reverse", "Result order for NMI query.php")
	cmd.Flags().Int("max-pages", 0, "Max pages to fetch (0 = unlimited)")
	cmd.Flags().Bool("apply", false, "Apply cancellation for local-only subscriptions")
	cmd.Flags().Bool("revoke-access", false, "Revoke entitlements immediately when applying cancellations")
	cmd.Flags().Bool("add-remote", false, "Create memberships for remote-only subscriptions (CCBill only)")
	cmd.Flags().Bool("allow-terminal-reactivation", false, "Allow terminal-to-active lifecycle transitions during reconciliation")
	cmd.Flags().String("ccbill-price-id", "", "Price ID to use when adding remote-only CCBill subscriptions")

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
	processor := viper.GetString("processor")
	processors := viper.GetStringSlice("processors")
	resultLimit := viper.GetInt("result-limit")
	resultOrder := viper.GetString("result-order")
	maxPages := viper.GetInt("max-pages")
	apply := viper.GetBool("apply")
	revokeAccess := viper.GetBool("revoke-access")
	addRemote := viper.GetBool("add-remote")
	allowTerminalReactivation := viper.GetBool("allow-terminal-reactivation")
	ccbillPriceID := viper.GetString("ccbill-price-id")

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

	ctx := context.Background()
	processorList := normalizeProcessorList(processors, processor)
	if len(processorList) == 0 {
		processorList = []string{"mobius"}
	}

	for _, name := range processorList {
		if err := reconcileProcessor(ctx, application, cfg, name, reconcileOptions{
			resultLimit:               resultLimit,
			resultOrder:               resultOrder,
			maxPages:                  maxPages,
			apply:                     apply,
			revokeAccess:              revokeAccess,
			addRemote:                 addRemote,
			allowTerminalReactivation: allowTerminalReactivation,
			ccbillPriceID:             ccbillPriceID,
		}); err != nil {
			return err
		}
	}

	return nil
}

func buildNMIClient(cfg *config.Config, processor string) (*nmi.NMIClient, error) {
	name := strings.TrimSpace(strings.ToLower(processor))
	if name == "" {
		name = "mobius"
	}

	nmiProcessors := cfg.GetNMIProcessors()
	procConfig, ok := nmiProcessors[name]
	if !ok || procConfig == nil {
		return nil, fmt.Errorf("nmi processor '%s' not configured", name)
	}

	settings := procConfig.ToNMIProviderSettings(name)
	if strings.TrimSpace(settings.SecurityKey) == "" {
		return nil, fmt.Errorf("nmi processor '%s' security key is required", name)
	}

	return nmi.NewClient(name, settings, cfg.IsTestMode())
}

func fetchCCBillSubscriptions(ctx context.Context, cfg *config.Config) ([]ccbill.CCBillRecord, error) {
	ccbillProc := cfg.GetCCBillProcessor()
	if ccbillProc == nil {
		return nil, fmt.Errorf("ccbill processor not configured")
	}
	client := ccbill.NewDataLinkClient(ccbillProc.ToCCBillConfig())
	if err := client.ValidateConfig(); err != nil {
		return nil, err
	}
	return client.FetchActiveMembers(ctx)
}

type reconcileOptions struct {
	resultLimit               int
	resultOrder               string
	maxPages                  int
	apply                     bool
	revokeAccess              bool
	addRemote                 bool
	allowTerminalReactivation bool
	ccbillPriceID             string
}

func reconcileProcessor(ctx context.Context, application *app.App, cfg *config.Config, processorName string, opts reconcileOptions) error {
	processorName = strings.TrimSpace(strings.ToLower(processorName))
	if processorName == "" {
		processorName = "mobius"
	}

	fmt.Printf("\n== %s ==\n", processorName)

	remoteIDs := make(map[string]struct{})
	remoteCount := 0
	ccbillRecords := map[string]ccbill.CCBillRecord{}

	if processorName == "ccbill" {
		records, err := fetchCCBillSubscriptions(ctx, cfg)
		if err != nil {
			return fmt.Errorf("fetch ccbill subscriptions failed: %w", err)
		}
		for _, record := range records {
			if record.Status != "Y" {
				continue
			}
			id := fmt.Sprintf("%d", record.SubscriptionID)
			remoteIDs[id] = struct{}{}
			ccbillRecords[id] = record
		}
		remoteCount = len(records)
	} else {
		client, err := buildNMIClient(cfg, processorName)
		if err != nil {
			return fmt.Errorf("nmi client init failed: %w", err)
		}
		remoteIDs, remoteCount, err = fetchRemoteSubscriptions(client, opts.resultLimit, opts.resultOrder, opts.maxPages)
		if err != nil {
			return fmt.Errorf("fetch remote subscriptions failed: %w", err)
		}
	}

	subService := subscriptions.NewSubscriptionService(application.Runtime.DB, nil, nil, nil, nil, nil)
	localSubs, err := subService.GetActiveSubscriptionsByProcessor(ctx, processorName)
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

	remoteOnly := diffKeys(remoteIDs, localIDs)
	localOnly := diffKeys(localIDs, remoteIDs)

	fmt.Printf("remote subscriptions fetched: %d (unique ids=%d)\n", remoteCount, len(remoteIDs))
	fmt.Printf("local active subscriptions: %d (unique ids=%d)\n", len(localSubs), len(localIDs))

	if len(remoteOnly) == 0 {
		fmt.Println("remote-only subscriptions: none")
	} else {
		fmt.Printf("remote-only subscriptions (%d):\n", len(remoteOnly))
		for _, id := range remoteOnly {
			fmt.Printf("  %s\n", id)
		}
	}

	if len(localOnly) == 0 {
		fmt.Println("local-only subscriptions: none")
	} else {
		fmt.Printf("local-only subscriptions (%d):\n", len(localOnly))
		for _, id := range localOnly {
			fmt.Printf("  %s\n", id)
		}
	}

	if opts.apply && len(localOnly) > 0 {
		if application.Runtime.SubscriptionLifecycleService == nil {
			return fmt.Errorf("subscription lifecycle service unavailable; cannot apply changes")
		}

		for _, id := range localOnly {
			sub, err := subService.GetByProcessorSubscriptionID(ctx, processorName, id)
			if err != nil || sub == nil {
				fmt.Fprintf(os.Stderr, "skip %s: failed to load subscription: %v\n", id, err)
				continue
			}

			cancelFeedback := "Cancelled via subscription-sync reconciliation"
			processorModel := models.Processor(processorName)
			err = application.Runtime.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
				RevokeAccess:            opts.revokeAccess,
				Processor:               &processorModel,
				ProcessorSubscriptionID: &id,
				CancelFeedback:          &cancelFeedback,
				SubscriptionID:          &sub.ID,
				CancelType:              models.CancelTypeMerchant,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to cancel %s: %v\n", id, err)
				continue
			}

			fmt.Printf("cancelled local subscription %s (revoke_access=%t)\n", id, opts.revokeAccess)
		}
	}

	if opts.addRemote && processorName == "ccbill" && len(remoteOnly) > 0 {
		if application.Runtime.SubscriptionLifecycleService == nil {
			return fmt.Errorf("subscription lifecycle service unavailable; cannot add memberships")
		}

		priceID, err := resolveCCBillPriceID(ctx, application.Runtime.DB, opts.ccbillPriceID)
		if err != nil {
			return err
		}
		priceService := catalog.NewPriceService(application.Runtime.DB)
		price, err := priceService.GetByID(ctx, priceID)
		if err != nil {
			return fmt.Errorf("load price for ccbill add-remote: %w", err)
		}

		profileRepo := repo.NewProfileRepo(application.Runtime.DB)
		for _, id := range remoteOnly {
			record, ok := ccbillRecords[id]
			if !ok {
				fmt.Fprintf(os.Stderr, "skip %s: missing CCBill record\n", id)
				continue
			}

			username := strings.TrimSpace(record.Username)
			if username == "" {
				fmt.Fprintf(os.Stderr, "skip %s: missing username in CCBill record\n", id)
				continue
			}

			userID, err := profileRepo.GetUserIDByUsername(ctx, username)
			if err != nil {
				fmt.Fprintf(os.Stderr, "skip %s: failed to resolve username %s: %v\n", id, username, err)
				continue
			}

			email := strings.TrimSpace(record.Email)
			var emailPtr *string
			if email != "" {
				emailCopy := email
				emailPtr = &emailCopy
			}

			existingSub, err := subService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorCCBill), id)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				fmt.Fprintf(os.Stderr, "skip %s: failed to load existing subscription: %v\n", id, err)
				continue
			}

			if existingSub != nil {
				if existingSub.Status != models.StatusActive {
					renewPrice := price
					if existingSub.PriceID != price.ID {
						renewPrice, err = priceService.GetByID(ctx, existingSub.PriceID)
						if err != nil {
							fmt.Fprintf(os.Stderr, "skip %s: failed to load price for renewal: %v\n", id, err)
							continue
						}
						if renewPrice == nil {
							fmt.Fprintf(os.Stderr, "skip %s: missing price for renewal\n", id)
							continue
						}
					}

					err = application.Runtime.SubscriptionLifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
						Processor:                 models.ProcessorCCBill,
						ProcessorSubscriptionID:   id,
						TransactionID:             fmt.Sprintf("ccbill-cmd-renew-%s", id),
						Amount:                    renewPrice.Amount,
						Currency:                  renewPrice.Currency,
						AllowTerminalReactivation: opts.allowTerminalReactivation,
					})
					if err != nil {
						fmt.Fprintf(os.Stderr, "failed to renew membership for %s: %v\n", id, err)
						continue
					}
					fmt.Printf("renewed membership for remote subscription %s\n", id)
					continue
				}

				fmt.Fprintf(os.Stderr, "skip %s: existing subscription already active\n", id)
				continue
			}

			_, err = application.Runtime.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
				UserID:                  userID,
				PriceID:                 priceID,
				Processor:               models.ProcessorCCBill,
				ProcessorSubscriptionID: &id,
				UserEmail:               emailPtr,
				TransactionID:           fmt.Sprintf("ccbill-cmd-%s", id),
				Amount:                  price.Amount,
				Currency:                price.Currency,
			})
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to add membership for %s: %v\n", id, err)
				continue
			}
			fmt.Printf("created membership for remote subscription %s\n", id)
		}
	}

	return nil
}

func normalizeProcessorList(processors []string, fallback string) []string {
	out := make([]string, 0)
	seen := make(map[string]struct{})
	add := func(value string) {
		value = strings.TrimSpace(strings.ToLower(value))
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}

	for _, entry := range processors {
		parts := strings.Split(entry, ",")
		for _, part := range parts {
			add(part)
		}
	}
	add(fallback)
	return out
}

func resolveCCBillPriceID(ctx context.Context, database *db.DB, rawID string) (uuid.UUID, error) {
	if strings.TrimSpace(rawID) != "" {
		parsed, err := uuid.Parse(strings.TrimSpace(rawID))
		if err != nil {
			return uuid.Nil, fmt.Errorf("invalid ccbill price id: %w", err)
		}
		return parsed, nil
	}

	priceService := catalog.NewPriceService(database)
	prices, err := priceService.GetAllActive(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("load active prices: %w", err)
	}

	var matches []uuid.UUID
	for _, price := range prices {
		if price.HasProcessor(models.ProcessorCCBill) {
			matches = append(matches, price.ID)
		}
	}

	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		return uuid.Nil, fmt.Errorf("no active CCBill prices found; set --ccbill-price-id")
	}
	return uuid.Nil, fmt.Errorf("multiple active CCBill prices found; set --ccbill-price-id")
}

func fetchRemoteSubscriptions(client *nmi.NMIClient, limit int, order string, maxPages int) (map[string]struct{}, int, error) {
	if limit <= 0 {
		limit = 100
	}
	result := make(map[string]struct{})
	totalSeen := 0

	for page := 0; ; page++ {
		if maxPages > 0 && page >= maxPages {
			break
		}

		resp, err := client.QueryRecurringSubscriptions(nmi.RecurringQueryParams{
			ResultLimit: limit,
			PageNumber:  page,
			ResultOrder: order,
		})
		if err != nil {
			return nil, totalSeen, err
		}

		subs, err := parseRecurringSubscriptions(resp)
		if err != nil {
			return nil, totalSeen, err
		}
		if len(subs) == 0 {
			break
		}

		totalSeen += len(subs)
		for _, id := range subs {
			result[id] = struct{}{}
		}

		if len(subs) < limit {
			break
		}
	}

	return result, totalSeen, nil
}

func parseRecurringSubscriptions(payload string) ([]string, error) {
	var resp nmResponse
	if err := xml.Unmarshal([]byte(payload), &resp); err != nil {
		return nil, fmt.Errorf("parse xml: %w", err)
	}

	result := make([]string, 0, len(resp.Subscriptions))
	for _, sub := range resp.Subscriptions {
		id := strings.TrimSpace(sub.SubscriptionID)
		if id != "" {
			result = append(result, id)
		}
	}
	return result, nil
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
