package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/tenant"
)

// newReconcileCmd wires the #107 processor-reconcile CLI:
//
//	billing reconcile check  [--provider=... --since=... --until=...]  advisory diff
//	billing reconcile fix    [--provider=... --since=... --until=...]  enforce (LOCAL writes only)
//	billing reconcile report [--run=ID]                                latest/specified run report
//
// Reconciliation is manual-only by design (no scheduled runs). The remote
// processors are NEVER mutated: fix converges local state and queues remote
// actions for an admin, so it is safe (and intended) under mode=readonly.
func newReconcileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Reconcile local billing state against the payment processors (#107)",
	}

	var (
		providers   []string
		since       string
		until       string
		format      string
		tenantSlug  string
		runIDStr    string
		materialize bool
	)
	addRunFlags := func(c *cobra.Command) {
		c.Flags().StringSliceVar(&providers, "provider", nil, "Provider(s) to reconcile: nmi, ccbill, stripe, solana (default: all configured)")
		c.Flags().StringVar(&since, "since", "", "Transaction window start (RFC3339 or YYYY-MM-DD)")
		c.Flags().StringVar(&until, "until", "", "Transaction window end (RFC3339 or YYYY-MM-DD)")
		c.Flags().StringVar(&format, "format", "table", "Output format: table, json")
		c.Flags().StringVar(&tenantSlug, "tenant", "", "Tenant slug or id (default: the default tenant)")
	}

	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Advisory reconcile: fetch + diff + persist findings, ZERO local writes",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileCLI(c, reconcile.ModeAdvisory, providers, since, until, format, tenantSlug, false)
		},
	}
	addRunFlags(checkCmd)

	fixCmd := &cobra.Command{
		Use:   "fix",
		Short: "Enforce reconcile: fetch + diff + apply idempotent LOCAL convergence writes (never touches a processor)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileCLI(c, reconcile.ModeEnforce, providers, since, until, format, tenantSlug, materialize)
		},
	}
	addRunFlags(fixCmd)
	fixCmd.Flags().BoolVar(&materialize, "materialize", false,
		"Bootstrap mode (explicit opt-in): auto-create local subscriptions for PS-1 findings whose identity AND plan resolve unambiguously; everything else stays admin_pending")

	reportCmd := &cobra.Command{
		Use:   "report",
		Short: "Show a run's summary, open findings, and dunning forensics (latest run by default)",
		RunE: func(c *cobra.Command, _ []string) error {
			return runReconcileReport(c, runIDStr, format, tenantSlug)
		},
	}
	reportCmd.Flags().StringVar(&runIDStr, "run", "", "Run ID (default: the latest run)")
	reportCmd.Flags().StringVar(&format, "format", "table", "Output format: table, json")
	reportCmd.Flags().StringVar(&tenantSlug, "tenant", "", "Tenant slug or id (default: the default tenant)")

	cmd.AddCommand(checkCmd, fixCmd, reportCmd)
	return cmd
}

// withReconcileApp bootstraps the application, resolves the tenant, and runs
// fn on a tenant-pinned connection so every engine read/write is RLS-scoped.
func withReconcileApp(cmd *cobra.Command, tenantSlug string, fn func(ctx context.Context, application *app.App) error) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	application, err := app.Bootstrap(cfg)
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() {
		if err := application.Close(context.Background()); err != nil {
			log.WithError(err).Error("Application cleanup failed")
		}
	}()

	tid, err := resolveReconcileTenant(cmd.Context(), application, tenantSlug)
	if err != nil {
		return err
	}
	ctx := tenant.WithID(cmd.Context(), tid)
	return application.Runtime.DB.RunInTenantConn(ctx, func(ctx context.Context) error {
		return fn(ctx, application)
	})
}

func resolveReconcileTenant(ctx context.Context, application *app.App, slug string) (tenant.ID, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return tenant.DefaultID, nil
	}
	if tid, err := tenant.ParseID(slug); err == nil {
		return tid, nil
	}
	var id string
	if err := application.Runtime.DB.Pool().
		QueryRow(ctx, `SELECT id::text FROM billing.tenants WHERE lower(slug) = lower($1)`, slug).
		Scan(&id); err != nil {
		return tenant.ID{}, fmt.Errorf("resolve tenant %q: %w", slug, err)
	}
	return tenant.ParseID(id)
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

func runReconcileCLI(cmd *cobra.Command, mode reconcile.Mode, providerNames []string, sinceStr, untilStr, format, tenantSlug string, materialize bool) error {
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

	return withReconcileApp(cmd, tenantSlug, func(ctx context.Context, application *app.App) error {
		rt := application.Runtime
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
			Mode:        mode,
			Providers:   provs,
			Since:       sinceT,
			Until:       untilT,
			Materialize: materialize,
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

func runReconcileReport(cmd *cobra.Command, runIDStr, format, tenantSlug string) error {
	return withReconcileApp(cmd, tenantSlug, func(ctx context.Context, application *app.App) error {
		store := &reconcile.PGStore{DB: application.Runtime.DB}

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
