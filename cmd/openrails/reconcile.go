package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newPullProviderCmd wires the #107/#511 provider-pull CLI:
//
//	openrails pull-provider [--provider=... --since=... --until=...] [--insert|--overwrite|--prune]
//	openrails pull-provider report [--run=ID] latest/specified run report
//
// Reconciliation is manual-only by design (no scheduled runs). The remote
// rails are NEVER mutated: mutation flags converge local state only.
// Remote-provider writes are queued by the operation that requests them through
// the provider intent ledger, not by provider pull.
func newPullProviderCmd() *cobra.Command {
	var (
		providers       []string
		since           string
		until           string
		format          string
		merchantSlug    string
		providerAccount string
		runIDStr        string
		logDir          string
		insert          bool
		overwrite       bool
		prune           bool
	)

	cmd := &cobra.Command{
		Use:   "pull-provider",
		Short: "Pull provider-observed state into the local mirror, then converge (#511). Plan-only unless mutation flags are set.",
		Long: "Pull provider-observed truth (subscriptions, payments, refunds, vault) into the local mirror, " +
			"then run a one-shot Converge(merchant).\n\n" +
			"Safety-first: a bare `pull-provider` is plan-only — it discovers every PULL-class divergence and logs " +
			"the changes it WOULD make, writing nothing. Mutation classes are explicit and compose: `--insert` imports " +
			"provider-observed records missing locally; `--overwrite` updates existing local mirror rows from provider " +
			"truth; `--prune` deletes eligible local subscriptions/payments attributed to the pulled provider account " +
			"that are ABSENT from the provider source. The remote rails are NEVER mutated.",
		RunE: func(c *cobra.Command, _ []string) error {
			return runPullProvider(c, providers, providerAccount, since, until, format, merchantSlug, logDir, insert, overwrite, prune)
		},
	}
	cmd.Flags().StringSliceVar(&providers, "provider", nil, "Provider(s) to pull: nmi, ccbill, stripe, solana (default: all configured)")
	cmd.Flags().StringVar(&providerAccount, "provider-account", "", "Provider account UUID to pull explicitly (requires matching configured credentials)")
	cmd.Flags().StringVar(&since, "since", "", "Transaction window start (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&until, "until", "", "Transaction window end (RFC3339 or YYYY-MM-DD)")
	cmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	cmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (required)")
	cmd.Flags().StringVar(&logDir, "log-dir", "openrails-pull-provider-logs", "Directory for pull-provider .log files")
	cmd.Flags().BoolVar(&insert, "insert", false, "Insert provider-observed records that are missing locally")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite existing local mirror rows from provider-observed truth")
	cmd.Flags().BoolVar(&prune, "prune", false, "Delete eligible local subscriptions/payments for the pulled provider account that are ABSENT from the provider source")

	reportCmd := &cobra.Command{
		Use:   "report",
		Short: "Show a run's summary, open findings, and dunning forensics (latest run by default merchant)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileReport(c, runIDStr, format, merchantSlug)
		},
	}
	reportCmd.Flags().StringVar(&runIDStr, "run", "", "Run ID (default: the latest run)")
	reportCmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	reportCmd.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (required)")

	cmd.AddCommand(reportCmd)
	return cmd
}

type reconcileRuntime struct {
	DB             *db.DB
	Config         *config.Config
	Rails          config.RailSet
	NMIClients     map[string]*nmi.NMIClient
	CCBillDataLink *ccbill.DataLinkClient
	SolanaRPC      *solanaint.RPCClient
}

// withReconcileRuntime builds only the dependencies reconciliation needs, then
// runs fn on a merchant-pinned connection so every engine read/write is
// RLS-scoped. It deliberately avoids app.Bootstrap: reconcile does not need
// Redis, health checks, River, cache monitoring, HTTP, or analytics emitters.
func withReconcileRuntime(cmd *cobra.Command, merchantSlug string, fn func(ctx context.Context, rt *reconcileRuntime) error) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	tid, err := resolveReconcileMerchantWithDB(cmd.Context(), cfg, merchantSlug)
	if err != nil {
		return err
	}
	rt, cleanup, err := newReconcileRuntime(cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	ctx := merchant.WithID(cmd.Context(), tid)
	return rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return fn(ctx, rt)
	})
}

func newReconcileRuntime(cfg *config.Config) (*reconcileRuntime, func(), error) {
	if cfg == nil || cfg.DB == nil {
		return nil, nil, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}
	cleanup := func() { _ = database.Close() }

	rails := config.RailSet{}
	nmiClients, err := reconcileNMIClients(cfg, rails)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	ccbillDataLink, err := reconcileCCBillDataLink(cfg, rails)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	solanaRPC, err := reconcileSolanaRPC(cfg, rails)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return &reconcileRuntime{
		DB:             database,
		Config:         cfg,
		Rails:          rails,
		NMIClients:     nmiClients,
		CCBillDataLink: ccbillDataLink,
		SolanaRPC:      solanaRPC,
	}, cleanup, nil
}

func reconcileNMIClients(cfg *config.Config, rails config.RailSet) (map[string]*nmi.NMIClient, error) {
	clients := map[string]*nmi.NMIClient{}
	for name, procConfig := range rails.GetNMIRails() {
		providerKey := strings.TrimSpace(strings.ToLower(name))
		if providerKey == "" {
			return nil, fmt.Errorf("nmi provider name cannot be empty")
		}
		if strings.TrimSpace(procConfig.SecurityKey) == "" {
			return nil, fmt.Errorf("nmi provider %q security key is required for reconciliation", providerKey)
		}
		client, err := nmi.NewClient(providerKey, procConfig.ToNMIProviderSettings(providerKey), cfg.IsTestMode())
		if err != nil {
			return nil, err
		}
		client.ReadOnly = true
		clients[providerKey] = client
	}
	return clients, nil
}

func reconcileCCBillDataLink(cfg *config.Config, rails config.RailSet) (*ccbill.DataLinkClient, error) {
	_, proc, err := rails.PrimaryRailByType(config.RailTypeCCBill)
	if err != nil {
		return nil, err
	}
	if proc == nil || proc.DataLinkUsername == "" || proc.DataLinkPassword == "" || proc.ClientAccNum == "" {
		return nil, nil
	}
	ccbillConfig := proc.ToCCBillConfig()
	ccbillConfig.TestMode = cfg.IsTestMode()
	return ccbill.NewDataLinkClient(ccbillConfig), nil
}

func reconcileSolanaRPC(cfg *config.Config, rails config.RailSet) (*solanaint.RPCClient, error) {
	_, proc, err := rails.PrimaryRailByType(config.RailTypeSolana)
	if err != nil {
		return nil, err
	}
	if proc == nil {
		return nil, nil
	}
	network := "mainnet"
	if cfg.IsTestMode() {
		network = "devnet"
	}
	return solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{
		HeliusAPIKey: proc.HeliusAPIKey,
		Network:      network,
		ReadOnly:     true,
	}), nil
}

func resolveReconcileMerchantWithDB(ctx context.Context, cfg *config.Config, slug string) (merchant.ID, error) {
	if cfg == nil || cfg.DB == nil {
		return merchant.ID{}, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return merchant.ID{}, fmt.Errorf("open postgres: %w", err)
	}
	defer func() {
		_ = database.Close()
	}()

	tid, err := resolveCLIMerchant(ctx, database, slug)
	if err != nil {
		if strings.HasPrefix(err.Error(), "--merchant is required") {
			return merchant.ID{}, fmt.Errorf("--merchant is required (slug or id of the merchant to reconcile)")
		}
		return merchant.ID{}, err
	}
	return tid, nil
}

func parseReconcileCLITime(s, name string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --%s %q (want RFC3339 or YYYY-MM-DD)", name, s)
	}
	return t, nil
}

func runPullProvider(cmd *cobra.Command, providerNames []string, providerAccountStr, sinceStr, untilStr, format, merchantSlug, logDir string, insert, overwrite, prune bool) error {
	// Plan-only is the safe default; local writes require explicit mutation flags.
	mode := reconcile.ModeAdvisory
	if insert || overwrite {
		mode = reconcile.ModeEnforce
	}
	sinceT, err := parseReconcileCLITime(sinceStr, "since")
	if err != nil {
		return err
	}
	untilT, err := parseReconcileCLITime(untilStr, "until")
	if err != nil {
		return err
	}
	var provs []reconcile.Provider
	for _, p := range providerNames {
		provs = append(provs, reconcile.Provider(strings.ToLower(strings.TrimSpace(p))))
	}

	return withReconcileRuntime(cmd, merchantSlug, func(ctx context.Context, rt *reconcileRuntime) error {
		providerKeys := map[reconcile.Provider]string{}
		explicitBindings := map[reconcile.Provider]reconcile.ProviderAccountBinding{}
		if strings.TrimSpace(providerAccountStr) != "" {
			provider, providerKey, binding, err := resolveReconcileProviderAccountTarget(ctx, rt, providerAccountStr)
			if err != nil {
				return err
			}
			if len(provs) == 0 {
				provs = []reconcile.Provider{provider}
			} else {
				for _, p := range provs {
					if p != provider {
						return fmt.Errorf("--provider-account %s is a %s account, but --provider includes %s", providerAccountStr, provider, p)
					}
				}
			}
			providerKeys[provider] = providerKey
			explicitBindings[provider] = binding
		}

		fetchers, err := reconcile.BuildFetchersWithOptions(rt.Config, rt.Rails, reconcile.FetcherClients{
			NMIClients:     rt.NMIClients,
			CCBillDataLink: rt.CCBillDataLink,
			SolanaRPC:      rt.SolanaRPC,
		}, rt.DB, reconcile.FetcherOptions{ProviderKeys: providerKeys})
		if err != nil {
			return err
		}
		if len(fetchers) == 0 {
			return fmt.Errorf("no payment providers configured for reconciliation")
		}
		bindings, err := reconcileProviderAccountBindings(ctx, rt, fetchers, provs, explicitBindings)
		if err != nil {
			return err
		}
		eng := reconcile.NewEngine(rt.DB, rt.Config, fetchers)

		res, runErr := eng.Run(ctx, reconcile.RunParams{
			Mode:             mode,
			Mutations:        &reconcile.LocalMutationPolicy{Insert: insert, Overwrite: overwrite},
			Providers:        provs,
			ProviderAccounts: bindings,
			Since:            sinceT,
			Until:            untilT,
		})
		if res == nil {
			return runErr
		}

		var pruneLogs []pullProviderPruneLog
		var pruneChanges []reconcile.MutationRecord
		// --prune (opt-in): delete local subscriptions/payments attributed to the
		// pulled provider account(s) that are ABSENT from the provider source.
		if prune && runErr == nil {
			for provider, binding := range bindings {
				fetcher, ok := fetchers[provider]
				if !ok {
					continue
				}
				pr, err := reconcile.PruneProviderAccountExcess(ctx, rt.DB, fetcher, provider, binding, reconcile.PruneParams{
					Since: sinceT, Until: untilT, Apply: true,
				})
				if err != nil {
					return fmt.Errorf("prune %s (%s): %w", provider, binding.AccountID, err)
				}
				pruneLogs = append(pruneLogs, pullProviderPruneLog{Provider: provider, Binding: binding, Result: pr})
				pruneChanges = append(pruneChanges, pruneMutationRecords(provider, pr, "applied")...)
			}
		}

		var convergeLog *pullProviderConvergeLog
		// After successful local mutations, run one idempotent Converge(merchant):
		// the mirror now reflects provider truth, so DERIVE/LIFE/CON converge grant
		// effects + lifecycle to it. Plan-only never converges (it wrote nothing).
		if (insert || overwrite || prune) && runErr == nil {
			merchantID, mErr := merchant.Require(ctx)
			if mErr != nil {
				return mErr
			}
			// A full HEAD pull across ALL providers proves provider-side absence,
			// so it marks the pulled source domains fully_reconciled — releasing
			// the confirmed-absence gate (§3.2) for destructive EXCESS repairs. A
			// provider-filtered or windowed (--since) pull does NOT prove full
			// absence, so it must not flip the gate.
			fullHead := len(provs) == 0 && sinceT.IsZero() && (insert || overwrite || prune)
			if fullHead {
				now := time.Now().UTC()
				for _, domain := range []string{"subscriptions", "payments"} {
					if _, e := rt.DB.Gen(ctx).UpsertReconciliationState(ctx, gen.UpsertReconciliationStateParams{
						MerchantID: merchantID.UUID(), SourceDomain: domain, FullyReconciled: true, LastFullPullAt: &now,
					}); e != nil {
						return fmt.Errorf("mark %s reconciled: %w", domain, e)
					}
				}
			}
			cres, cErr := converge.NewConvergeEngine(rt.DB).Converge(ctx, converge.Scope{Merchant: merchantID})
			if cErr != nil {
				return fmt.Errorf("post-pull converge: %w", cErr)
			}
			convergeLog = &pullProviderConvergeLog{
				RunID:             cres.RunID,
				Findings:          cres.Findings,
				AutoFixed:         cres.AutoFixed,
				ReconcileRequired: cres.ReconcileRequired,
				RequiresReview:    cres.RequiresReview,
				AdminRequired:     cres.AdminRequired,
			}
		}

		store := &reconcile.PGStore{DB: rt.DB}
		run, err := store.GetRun(ctx, res.RunID)
		if err != nil {
			return err
		}
		appliedChanges := append([]reconcile.MutationRecord(nil), res.AppliedChanges...)
		appliedChanges = append(appliedChanges, pruneChanges...)
		logPath, err := writePullProviderLog(logDir, run, res, appliedChanges, pruneLogs, convergeLog, pullProviderMutationFlags{
			Insert: insert, Overwrite: overwrite, Prune: prune,
		})
		if err != nil {
			return err
		}
		if err := renderPullProviderStdout(os.Stdout, format, logPath, run, res, appliedChanges, pruneLogs, convergeLog); err != nil {
			return err
		}
		return runErr // non-nil when a provider failed/aborted -> non-zero exit
	})
}

func resolveReconcileProviderAccountTarget(ctx context.Context, rt *reconcileRuntime, providerAccountStr string) (reconcile.Provider, string, reconcile.ProviderAccountBinding, error) {
	id, err := uuid.Parse(strings.TrimSpace(providerAccountStr))
	if err != nil {
		return "", "", reconcile.ProviderAccountBinding{}, fmt.Errorf("invalid --provider-account UUID: %w", err)
	}
	account, err := rt.DB.Gen(ctx).GetProviderAccount(ctx, id)
	if err != nil {
		return "", "", reconcile.ProviderAccountBinding{}, fmt.Errorf("load provider account %s: %w", id, err)
	}
	provider := reconcile.Provider(account.ProviderType)
	// Provider-account identity is operator-declared (#592): select the configured
	// rail by provider type rather than resolving it from live credentials.
	providerKey := account.ProviderType
	if rt.Rails != nil {
		if key, _, kerr := rt.Rails.PrimaryRailByType(account.ProviderType); kerr == nil && key != "" {
			providerKey = key
		}
	}
	return provider, providerKey, reconcile.ProviderAccountBinding{
		ID:           account.ID,
		ProviderType: account.ProviderType,
		AccountID:    account.AccountID,
	}, nil
}

func reconcileProviderAccountBindings(_ context.Context, _ *reconcileRuntime, _ map[reconcile.Provider]reconcile.RailFetcher, _ []reconcile.Provider, explicit map[reconcile.Provider]reconcile.ProviderAccountBinding) (map[reconcile.Provider]reconcile.ProviderAccountBinding, error) {
	// Runtime provider-account identity resolution was removed (#592). Only
	// explicitly targeted (--provider-account) bindings are honored; otherwise
	// reconcile runs account-agnostic (the historical default). provider_accounts
	// is now an operator-declared catalog, no longer auto-bound from live creds.
	out := make(map[reconcile.Provider]reconcile.ProviderAccountBinding, len(explicit))
	for provider, binding := range explicit {
		out[provider] = binding
	}
	return out, nil
}

func runReconcileReport(cmd *cobra.Command, runIDStr, format, merchantSlug string) error {
	return withReconcileDB(cmd, merchantSlug, func(ctx context.Context, database *db.DB) error {
		store := &reconcile.PGStore{DB: database}

		var (
			run reconcile.RunRecord
			err error
		)
		if strings.TrimSpace(runIDStr) != "" {
			id, perr := uuid.Parse(strings.TrimSpace(runIDStr))
			if perr != nil {
				return fmt.Errorf("invalid --run id: %w", perr)
			}
			run, err = store.GetRun(ctx, id)
		} else {
			run, err = store.GetLatestRun(ctx)
		}
		if err != nil {
			return fmt.Errorf("load run: %w", err)
		}

		// The report shows the standing actionable findings (incl. the admin queue),
		// not just the ones touched by that run.
		var actionable []reconcile.FindingRecord
		for _, status := range []string{string(reconcile.FindingStatusReconcileRequired), string(reconcile.FindingStatusAdminRequired)} {
			batch, err := store.ListFindings(ctx, reconcile.FindingFilter{Status: status, Limit: 500})
			if err != nil {
				return err
			}
			actionable = append(actionable, batch...)
		}

		if format == "json" {
			return reconcile.RenderRunJSON(os.Stdout, run, actionable)
		}
		return reconcile.RenderRunTable(os.Stdout, run, actionable)
	})
}

func withReconcileDB(cmd *cobra.Command, merchantSlug string, fn func(ctx context.Context, database *db.DB) error) error {
	cfg, _ := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() {
		_ = database.Close()
	}()

	tid, err := resolveCLIMerchant(cmd.Context(), database, merchantSlug)
	if err != nil {
		if strings.HasPrefix(err.Error(), "--merchant is required") {
			return fmt.Errorf("--merchant is required (slug or id of the merchant to reconcile)")
		}
		return err
	}
	ctx := merchant.WithID(cmd.Context(), tid)
	return database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		return fn(ctx, database)
	})
}
