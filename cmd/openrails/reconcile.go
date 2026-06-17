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
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/merchant"
)

// newReconcileCmd wires the #107 processor-reconcile CLI:
//
//	openrails reconcile check  [--provider=... --since=... --until=...]  advisory diff
//	openrails reconcile fix    [--provider=... --since=... --until=...]  enforce (LOCAL writes only, incl. PS-1 materialization)
//	openrails reconcile report [--run=ID]                                latest/specified run report
//
// Reconciliation is manual-only by design (no scheduled runs). The remote
// processors are NEVER mutated: fix converges local state only. Remote-provider
// writes are queued by the operation that requests them through the provider
// intent ledger, not by reconciliation, so reconcile is safe under
// mode=readonly.
func newReconcileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile local billing state against the payment processors (#107)",
	}

	var (
		providers    []string
		since        string
		until        string
		format       string
		merchantSlug string
		runIDStr     string
	)
	addRunFlags := func(c *cobra.Command) {
		c.Flags().StringSliceVar(&providers, "provider", nil, "Provider(s) to reconcile: nmi, ccbill, stripe, solana (default: all configured)")
		c.Flags().StringVar(&since, "since", "", "Transaction window start (RFC3339 or YYYY-MM-DD)")
		c.Flags().StringVar(&until, "until", "", "Transaction window end (RFC3339 or YYYY-MM-DD)")
		c.Flags().StringVar(&format, "format", "table", "Output format: table, json")
		c.Flags().StringVar(&merchantSlug, "merchant", "", "Merchant slug or id (required)")
	}

	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Advisory reconcile: fetch + diff + persist findings, ZERO local writes",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileCLI(c, reconcile.ModeAdvisory, providers, since, until, format, merchantSlug)
		},
	}
	addRunFlags(checkCmd)

	fixCmd := &cobra.Command{
		Use:   "fix",
		Short: "Enforce reconcile: fetch + diff + apply idempotent LOCAL convergence writes (never touches a processor); PS-1 findings whose identity AND plan resolve unambiguously are materialized as local subscriptions, the rest stay admin_pending",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileCLI(c, reconcile.ModeEnforce, providers, since, until, format, merchantSlug)
		},
	}
	addRunFlags(fixCmd)

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

	cmd.AddCommand(checkCmd, fixCmd, reportCmd)
	return cmd
}

type reconcileRuntime struct {
	DB             *db.DB
	Config         *config.Config
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

	nmiClients, err := reconcileNMIClients(cfg)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return &reconcileRuntime{
		DB:             database,
		Config:         cfg,
		NMIClients:     nmiClients,
		CCBillDataLink: reconcileCCBillDataLink(cfg),
		SolanaRPC:      reconcileSolanaRPC(cfg),
	}, cleanup, nil
}

func reconcileNMIClients(cfg *config.Config) (map[string]*nmi.NMIClient, error) {
	clients := map[string]*nmi.NMIClient{}
	for name, procConfig := range cfg.GetNMIProcessors() {
		providerKey := strings.TrimSpace(strings.ToLower(name))
		if providerKey == "" {
			return nil, fmt.Errorf("nmi provider name cannot be empty")
		}
		if strings.TrimSpace(procConfig.SecurityKey) == "" {
			return nil, fmt.Errorf("nmi provider %q security key is required for reconciliation", providerKey)
		}
		client, err := nmi.NewClient(providerKey, procConfig.ToNMIProviderSettings(providerKey), cfg.IsTestEnv())
		if err != nil {
			return nil, err
		}
		client.ReadOnly = true
		client.SubscriptionDeletesDisabled = true
		clients[providerKey] = client
	}
	return clients, nil
}

func reconcileCCBillDataLink(cfg *config.Config) *ccbill.DataLinkClient {
	proc := cfg.GetCCBillProcessor()
	if proc == nil || proc.DataLinkUsername == "" || proc.DataLinkPassword == "" || proc.ClientAccNum == "" {
		return nil
	}
	ccbillConfig := proc.ToCCBillConfig()
	ccbillConfig.TestMode = cfg.IsTestEnv()
	return ccbill.NewDataLinkClient(ccbillConfig)
}

func reconcileSolanaRPC(cfg *config.Config) *solanaint.RPCClient {
	proc := cfg.GetSolanaProcessor()
	if proc == nil {
		return nil
	}
	network := "mainnet"
	if cfg.IsTestEnv() {
		network = "devnet"
	}
	return solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{
		HeliusAPIKey: proc.HeliusAPIKey,
		Network:      network,
		ReadOnly:     true,
	})
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

func runReconcileCLI(cmd *cobra.Command, mode reconcile.Mode, providerNames []string, sinceStr, untilStr, format, merchantSlug string) error {
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
		fetchers := reconcile.BuildFetchers(rt.Config, reconcile.FetcherClients{
			NMIClients:     rt.NMIClients,
			CCBillDataLink: rt.CCBillDataLink,
			SolanaRPC:      rt.SolanaRPC,
		}, rt.DB)
		if len(fetchers) == 0 {
			return fmt.Errorf("no payment providers configured for reconciliation")
		}
		eng := reconcile.NewEngine(rt.DB, rt.Config, fetchers)

		res, runErr := eng.Run(ctx, reconcile.RunParams{
			Mode:      mode,
			Providers: provs,
			Since:     sinceT,
			Until:     untilT,
		})
		if res == nil {
			return runErr
		}

		store := &reconcile.PGStore{DB: rt.DB}
		run, err := store.GetRun(ctx, res.RunID)
		if err != nil {
			return err
		}
		if format == "json" {
			if err := reconcile.RenderRunJSON(os.Stdout, run, res.Findings); err != nil {
				return err
			}
		} else if err := reconcile.RenderRunTable(os.Stdout, run, res.Findings); err != nil {
			return err
		}
		return runErr // non-nil when a provider failed/aborted -> non-zero exit
	})
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

		// The report shows the standing OPEN findings (incl. the admin queue),
		// not just the ones touched by that run.
		var open []reconcile.FindingRecord
		for _, status := range []string{string(reconcile.FindingStatusOpen), string(reconcile.FindingStatusAdminPending)} {
			batch, err := store.ListFindings(ctx, reconcile.FindingFilter{Status: status, Limit: 500})
			if err != nil {
				return err
			}
			open = append(open, batch...)
		}

		if format == "json" {
			return reconcile.RenderRunJSON(os.Stdout, run, open)
		}
		return reconcile.RenderRunTable(os.Stdout, run, open)
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
