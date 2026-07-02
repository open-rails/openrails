package embedded

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

// PullProviderOptions mirrors `openrails pull-provider` for embedded hosts.
type PullProviderOptions struct {
	Config              *config.Config
	MerchantSlug        string
	Providers           []string
	RailMerchantAccount string
	Since               string
	Until               string
	Format              string
	LogDir              string
	Insert              bool
	Overwrite           bool
	Prune               bool
	Out                 io.Writer
}

// PullProviderReportOptions mirrors `openrails pull-provider report`.
type PullProviderReportOptions struct {
	Config       *config.Config
	MerchantSlug string
	RunID        string
	Format       string
	Out          io.Writer
}

// PullProvider pulls provider-observed state into OpenRails' local mirror.
// Bare calls are dry-run only; local writes require Insert, Overwrite, or Prune.
func PullProvider(ctx context.Context, opts PullProviderOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Format == "" {
		opts.Format = "table"
	}
	mode := reconcile.ModeAdvisory
	if opts.Insert || opts.Overwrite {
		mode = reconcile.ModeEnforce
	}
	sinceT, err := parsePullProviderTime(opts.Since, "since")
	if err != nil {
		return err
	}
	untilT, err := parsePullProviderTime(opts.Until, "until")
	if err != nil {
		return err
	}
	var providers []reconcile.Provider
	for _, p := range opts.Providers {
		providers = append(providers, reconcile.Provider(strings.ToLower(strings.TrimSpace(p))))
	}

	rt, cleanup, err := newPullProviderRuntime(opts.Config)
	if err != nil {
		return err
	}
	defer cleanup()

	merchantID, err := resolvePullProviderMerchant(ctx, rt.DB, opts.MerchantSlug)
	if err != nil {
		return err
	}
	ctx = merchant.WithID(ctx, merchantID)
	return rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		providerKeys := map[reconcile.Provider]string{}
		explicitBindings := map[reconcile.Provider]reconcile.RailMerchantAccountBinding{}
		if strings.TrimSpace(opts.RailMerchantAccount) != "" {
			provider, providerKey, binding, err := resolvePullRailMerchantAccountTarget(ctx, rt, opts.RailMerchantAccount)
			if err != nil {
				return err
			}
			if len(providers) == 0 {
				providers = []reconcile.Provider{provider}
			} else {
				for _, p := range providers {
					if p != provider {
						return fmt.Errorf("--provider-account %s is a %s account, but --provider includes %s", opts.RailMerchantAccount, provider, p)
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
		bindings := make(map[reconcile.Provider]reconcile.RailMerchantAccountBinding, len(explicitBindings))
		for provider, binding := range explicitBindings {
			bindings[provider] = binding
		}

		eng := reconcile.NewEngine(rt.DB, rt.Config, fetchers)
		res, runErr := eng.Run(ctx, reconcile.RunParams{
			Mode:                 mode,
			Mutations:            &reconcile.LocalMutationPolicy{Insert: opts.Insert, Overwrite: opts.Overwrite},
			Providers:            providers,
			RailMerchantAccounts: bindings,
			Since:                sinceT,
			Until:                untilT,
		})
		if res == nil {
			return runErr
		}

		var pruneLogs []pullProviderPruneLog
		var pruneChanges []reconcile.MutationRecord
		if opts.Prune && runErr == nil {
			for provider, binding := range bindings {
				fetcher, ok := fetchers[provider]
				if !ok {
					continue
				}
				pr, err := reconcile.PruneRailMerchantAccountExcess(ctx, rt.DB, fetcher, provider, binding, reconcile.PruneParams{
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
		if (opts.Insert || opts.Overwrite || opts.Prune) && runErr == nil {
			// #665 §3.2 confirmed-absence gate: flip reconciliation_state only for
			// the domains this pull PROVED (exhaustive coverage of every declared
			// rail account), never blindly. A full-head --insert --overwrite pull
			// remains the manual bulk-import way to establish the gate.
			if opts.Insert && opts.Overwrite {
				if _, err := reconcile.MarkReconciledSourceDomains(ctx, rt.DB.Gen(ctx), merchantID.UUID(), res.PullProofs(), time.Now().UTC()); err != nil {
					return fmt.Errorf("mark reconciled domains: %w", err)
				}
			}
			cres, err := converge.NewConvergeEngine(rt.DB).Converge(ctx, converge.Scope{Merchant: merchantID})
			if err != nil {
				return fmt.Errorf("post-pull converge: %w", err)
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
		logPath, err := writePullProviderLog(opts.LogDir, run, res, appliedChanges, pruneLogs, convergeLog, pullProviderMutationFlags{
			Insert: opts.Insert, Overwrite: opts.Overwrite, Prune: opts.Prune,
		})
		if err != nil {
			return err
		}
		if err := renderPullProviderStdout(opts.Out, opts.Format, logPath, run, res, appliedChanges, pruneLogs, convergeLog); err != nil {
			return err
		}
		return runErr
	})
}

// PullProviderReport renders a provider-pull run report for embedded hosts.
func PullProviderReport(ctx context.Context, opts PullProviderReportOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Format == "" {
		opts.Format = "table"
	}
	if opts.Config == nil || opts.Config.DB == nil {
		return fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(opts.Config.DB)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() {
		_ = database.Close()
	}()

	merchantID, err := resolvePullProviderMerchant(ctx, database, opts.MerchantSlug)
	if err != nil {
		return err
	}
	ctx = merchant.WithID(ctx, merchantID)
	return database.RunInMerchantConn(ctx, func(ctx context.Context) error {
		store := &reconcile.PGStore{DB: database}
		var run reconcile.RunRecord
		if strings.TrimSpace(opts.RunID) != "" {
			id, err := uuid.Parse(strings.TrimSpace(opts.RunID))
			if err != nil {
				return fmt.Errorf("invalid --run id: %w", err)
			}
			run, err = store.GetRun(ctx, id)
			if err != nil {
				return fmt.Errorf("load run: %w", err)
			}
		} else {
			run, err = store.GetLatestRun(ctx)
			if err != nil {
				return fmt.Errorf("load run: %w", err)
			}
		}

		var actionable []reconcile.FindingRecord
		for _, status := range []string{string(reconcile.FindingStatusReconcileRequired), string(reconcile.FindingStatusAdminRequired)} {
			batch, err := store.ListFindings(ctx, reconcile.FindingFilter{Status: status, Limit: 500})
			if err != nil {
				return err
			}
			actionable = append(actionable, batch...)
		}

		if opts.Format == "json" {
			return reconcile.RenderRunJSON(opts.Out, run, actionable)
		}
		return reconcile.RenderRunTable(opts.Out, run, actionable)
	})
}

type pullProviderRuntime struct {
	DB             *db.DB
	Config         *config.Config
	Rails          config.RailMerchantAccountSet
	NMIClients     map[string]*nmi.NMIClient
	CCBillDataLink *ccbill.DataLinkClient
	SolanaRPC      *solanaint.RPCClient
}

func newPullProviderRuntime(cfg *config.Config) (*pullProviderRuntime, func(), error) {
	if cfg == nil || cfg.DB == nil {
		return nil, nil, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(cfg.DB)
	if err != nil {
		return nil, nil, fmt.Errorf("open postgres: %w", err)
	}
	cleanup := func() { _ = database.Close() }
	rails := config.RailMerchantAccountSet{}
	nmiClients := map[string]*nmi.NMIClient{}
	ccbillDataLink, err := pullProviderCCBillDataLink(cfg, rails)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	solanaRPC, err := pullProviderSolanaRPC(cfg, rails)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return &pullProviderRuntime{
		DB:             database,
		Config:         cfg,
		Rails:          rails,
		NMIClients:     nmiClients,
		CCBillDataLink: ccbillDataLink,
		SolanaRPC:      solanaRPC,
	}, cleanup, nil
}

func pullProviderCCBillDataLink(cfg *config.Config, rails config.RailMerchantAccountSet) (*ccbill.DataLinkClient, error) {
	_, proc, err := rails.ActiveRailByType(models.RailCCBill)
	if err != nil {
		return nil, err
	}
	if proc == nil || proc.CCBill == nil || proc.CCBill.DataLinkUsername == "" || proc.CCBill.DataLinkPassword == "" || proc.CCBill.ClientAccNum == "" {
		return nil, nil
	}
	ccbillConfig := proc.ToCCBillConfig()
	ccbillConfig.TestMode = cfg.IsTestMode()
	return ccbill.NewDataLinkClient(ccbillConfig), nil
}

func pullProviderSolanaRPC(cfg *config.Config, rails config.RailMerchantAccountSet) (*solanaint.RPCClient, error) {
	_, proc, err := rails.ActiveRailByType(models.RailSolana)
	if err != nil {
		return nil, err
	}
	if proc == nil {
		return nil, nil
	}
	rpcProvider := ""
	rpcAPIKey := ""
	if proc.Solana != nil {
		rpcProvider = proc.Solana.RPCProvider
		rpcAPIKey = proc.Solana.RPCAPIKey
	}
	network := "mainnet"
	if cfg.IsTestMode() {
		network = "devnet"
	}
	return solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{
		RPCProvider: rpcProvider,
		RPCAPIKey:   rpcAPIKey,
		Network:     network,
		ReadOnly:    true,
	}), nil
}

func resolvePullProviderMerchant(ctx context.Context, database *db.DB, slug string) (merchant.ID, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return merchant.ID{}, fmt.Errorf("--merchant is required (slug or id)")
	}
	if id, err := merchant.ParseID(slug); err == nil {
		return id, nil
	}
	var id string
	if err := database.DataPool().QueryRow(ctx, `SELECT id::text FROM openrails.merchants WHERE lower(slug) = lower($1)`, slug).Scan(&id); err != nil {
		return merchant.ID{}, fmt.Errorf("resolve merchant %q: %w", slug, err)
	}
	return merchant.ParseID(id)
}

func resolvePullRailMerchantAccountTarget(ctx context.Context, rt *pullProviderRuntime, railMerchantAccountStr string) (reconcile.Provider, string, reconcile.RailMerchantAccountBinding, error) {
	id, err := uuid.Parse(strings.TrimSpace(railMerchantAccountStr))
	if err != nil {
		return "", "", reconcile.RailMerchantAccountBinding{}, fmt.Errorf("invalid --provider-account UUID: %w", err)
	}
	account, err := rt.DB.Gen(ctx).GetRailMerchantAccount(ctx, id)
	if err != nil {
		return "", "", reconcile.RailMerchantAccountBinding{}, fmt.Errorf("load provider account %s: %w", id, err)
	}
	provider := reconcile.Provider(account.Rail)
	providerKey := account.Rail
	if rt.Rails != nil {
		if key, _, err := rt.Rails.ActiveRailByType(models.Rail(account.Rail)); err == nil && key != "" {
			providerKey = key
		}
	}
	return provider, providerKey, reconcile.RailMerchantAccountBinding{
		ID:        account.ID,
		Rail:      account.Rail,
		AccountID: account.AccountID,
	}, nil
}

func parsePullProviderTime(s, name string) (time.Time, error) {
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

type pullProviderMutationFlags struct {
	Insert    bool
	Overwrite bool
	Prune     bool
}

type pullProviderPruneLog struct {
	Provider reconcile.Provider
	Binding  reconcile.RailMerchantAccountBinding
	Result   reconcile.PruneResult
}

type pullProviderConvergeLog struct {
	RunID             *uuid.UUID
	Findings          int
	AutoFixed         int
	ReconcileRequired int
	RequiresReview    int `json:"requires_review"`
	AdminRequired     int `json:"-"`
}

func writePullProviderLog(logDir string, run reconcile.RunRecord, res *reconcile.RunResult, appliedChanges []reconcile.MutationRecord, pruneLogs []pullProviderPruneLog, convergeLog *pullProviderConvergeLog, flags pullProviderMutationFlags) (string, error) {
	if strings.TrimSpace(logDir) == "" {
		logDir = "."
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("create pull-provider log dir: %w", err)
	}
	path := filepath.Join(logDir, fmt.Sprintf("pull-provider-%s.log", run.ID))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("open pull-provider log: %w", err)
	}
	defer func() { _ = f.Close() }()
	now := time.Now().UTC()
	writeLogLine(f, now,
		lf("event", "run"), lf("run_id", run.ID.String()), lf("status", run.Status),
		lf("mode", string(run.Mode)), lf("providers", strings.Join(run.Providers, ",")),
		lf("insert", strconv.FormatBool(flags.Insert)), lf("overwrite", strconv.FormatBool(flags.Overwrite)),
		lf("prune", strconv.FormatBool(flags.Prune)), lfTime("started_at", run.StartedAt),
		lfTimePtr("finished_at", run.FinishedAt), lfTimePtr("window_since", run.WindowSince),
		lfTimePtr("window_until", run.WindowUntil),
	)
	for _, rec := range res.Findings {
		writeLogLine(f, now,
			lf("event", "finding"), lf("run_id", run.ID.String()), lf("finding_id", rec.ID.String()),
			lf("provider", string(rec.Provider)), lf("type", string(rec.Type)), lf("status", string(rec.Status)),
			lf("severity", string(rec.Severity)), lf("subject_key", rec.SubjectKey), lfTime("last_seen_at", rec.LastSeenAt),
		)
	}
	for _, rec := range res.PlannedChanges {
		writeMutationLogLine(f, now, run.ID, rec)
	}
	for _, rec := range appliedChanges {
		writeMutationLogLine(f, now, run.ID, rec)
	}
	for _, pl := range pruneLogs {
		subReason := pl.Result.SubscriptionSkipReason
		if subReason == "" {
			subReason = "grant_ledger_entangled"
		}
		for _, id := range pl.Result.SkippedSubscriptionIDs {
			writeLogLine(f, now, lf("event", "prune_skip"), lf("run_id", run.ID.String()), lf("provider", string(pl.Provider)), lf("rail_merchant_account_id", pl.Binding.ID.String()), lf("table", "subscriptions"), lf("row_id", id.String()), lf("reason", subReason))
		}
		payReason := pl.Result.PaymentSkipReason
		if payReason == "" {
			payReason = "protected_dependents"
		}
		for _, id := range pl.Result.SkippedPaymentIDs {
			writeLogLine(f, now, lf("event", "prune_skip"), lf("run_id", run.ID.String()), lf("provider", string(pl.Provider)), lf("rail_merchant_account_id", pl.Binding.ID.String()), lf("table", "payments"), lf("row_id", id.String()), lf("reason", payReason))
		}
	}
	if convergeLog != nil {
		fields := []logField{
			lf("event", "converge"), lf("run_id", run.ID.String()), lf("findings", strconv.Itoa(convergeLog.Findings)),
			lf("auto_fixed", strconv.Itoa(convergeLog.AutoFixed)), lf("reconcile_required", strconv.Itoa(convergeLog.ReconcileRequired)),
			lf("requires_review", strconv.Itoa(convergeLog.RequiresReview)),
		}
		if convergeLog.RunID != nil {
			fields = append(fields, lf("converge_run_id", convergeLog.RunID.String()))
		}
		writeLogLine(f, now, fields...)
	}
	counts := summarizeMutations(res.PlannedChanges, appliedChanges)
	writeLogLine(f, now, lf("event", "summary"), lf("run_id", run.ID.String()), lf("findings", strconv.Itoa(len(res.Findings))), lf("planned", formatMutationCounts(counts["planned"])), lf("applied", formatMutationCounts(counts["applied"])))
	return path, nil
}

func pruneMutationRecords(provider reconcile.Provider, pr reconcile.PruneResult, phase string) []reconcile.MutationRecord {
	var out []reconcile.MutationRecord
	for _, id := range pr.SubscriptionIDs {
		out = append(out, reconcile.MutationRecord{Phase: phase, Provider: provider, Table: "subscriptions", Operation: "delete", RowID: id.String(), RowsAffected: 1})
	}
	for _, id := range pr.PaymentIDs {
		out = append(out, reconcile.MutationRecord{Phase: phase, Provider: provider, Table: "payments", Operation: "delete", RowID: id.String(), RowsAffected: 1})
	}
	return out
}

func renderPullProviderStdout(w io.Writer, format, logPath string, run reconcile.RunRecord, res *reconcile.RunResult, appliedChanges []reconcile.MutationRecord, pruneLogs []pullProviderPruneLog, convergeLog *pullProviderConvergeLog) error {
	counts := summarizeMutations(res.PlannedChanges, appliedChanges)
	statusCounts := findingStatusCounts(res.Findings)
	pruneCounts := summarizePrune(pruneLogs)
	if format == "json" {
		return json.NewEncoder(w).Encode(map[string]any{
			"run_id": run.ID, "status": run.Status, "log_file": logPath,
			"findings": statusCounts, "planned_changes": counts["planned"],
			"applied_changes": counts["applied"], "prune": pruneCounts, "converge": convergeLog,
		})
	}
	fmt.Fprintf(w, "pull-provider run %s status=%s\n", run.ID, run.Status)
	fmt.Fprintf(w, "findings: %s\n", formatFindingStatusCounts(statusCounts))
	fmt.Fprintf(w, "planned: %s\n", formatMutationCounts(counts["planned"]))
	fmt.Fprintf(w, "applied: %s\n", formatMutationCounts(counts["applied"]))
	if len(pruneLogs) > 0 {
		fmt.Fprintf(w, "prune: %s\n", formatPruneCounts(pruneCounts))
	}
	if convergeLog != nil {
		fmt.Fprintf(w, "converge: %d finding(s), %d auto-fixed, %d reconcile-required, %d requires-review\n", convergeLog.Findings, convergeLog.AutoFixed, convergeLog.ReconcileRequired, convergeLog.RequiresReview)
	}
	fmt.Fprintf(w, "log file: %s\n", logPath)
	return nil
}

type pruneCounts map[string]map[string]int
type mutationCounts map[string]map[string]int

func summarizePrune(logs []pullProviderPruneLog) pruneCounts {
	out := pruneCounts{}
	add := func(table, key string, n int) {
		if n <= 0 {
			return
		}
		if out[table] == nil {
			out[table] = map[string]int{}
		}
		out[table][key] += n
	}
	for _, log := range logs {
		add("subscriptions", "deleted", log.Result.Subscriptions)
		add("subscriptions", "skipped", log.Result.SubscriptionsSkipped)
		add("payments", "deleted", log.Result.Payments)
		add("payments", "skipped", log.Result.PaymentsSkipped)
	}
	return out
}

func summarizeMutations(planned, applied []reconcile.MutationRecord) map[string]mutationCounts {
	out := map[string]mutationCounts{"planned": {}, "applied": {}}
	add := func(phase string, rec reconcile.MutationRecord) {
		if rec.Table == "" || rec.Operation == "" {
			return
		}
		n := rec.RowsAffected
		if n <= 0 {
			n = 1
		}
		if out[phase][rec.Table] == nil {
			out[phase][rec.Table] = map[string]int{}
		}
		out[phase][rec.Table][rec.Operation] += n
	}
	for _, rec := range planned {
		add("planned", rec)
	}
	for _, rec := range applied {
		add("applied", rec)
	}
	return out
}

func findingStatusCounts(findings []reconcile.FindingRecord) map[string]int {
	out := map[string]int{}
	for _, rec := range findings {
		out[string(rec.Status)]++
	}
	return out
}

func formatFindingStatusCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, ", ")
}

func formatPruneCounts(counts pruneCounts) string {
	if len(counts) == 0 {
		return "none"
	}
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	var parts []string
	for _, table := range tables {
		var values []string
		for _, key := range []string{"deleted", "skipped"} {
			if n := counts[table][key]; n > 0 {
				values = append(values, fmt.Sprintf("%s=%d", key, n))
			}
		}
		if len(values) > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", table, strings.Join(values, " ")))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func formatMutationCounts(counts mutationCounts) string {
	if len(counts) == 0 {
		return "none"
	}
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	var tableParts []string
	for _, table := range tables {
		var opParts []string
		for _, op := range []string{"insert", "update", "delete"} {
			if n := counts[table][op]; n > 0 {
				opParts = append(opParts, fmt.Sprintf("%s=%d", pastTense(op), n))
			}
		}
		if len(opParts) > 0 {
			tableParts = append(tableParts, fmt.Sprintf("%s %s", table, strings.Join(opParts, " ")))
		}
	}
	if len(tableParts) == 0 {
		return "none"
	}
	return strings.Join(tableParts, "; ")
}

func pastTense(op string) string {
	switch op {
	case "insert":
		return "inserted"
	case "update":
		return "updated"
	case "delete":
		return "deleted"
	default:
		return op
	}
}

func writeMutationLogLine(w io.Writer, ts time.Time, runID uuid.UUID, rec reconcile.MutationRecord) {
	fields := []logField{
		lf("event", "mutation"), lf("run_id", runID.String()), lf("phase", rec.Phase),
		lf("provider", string(rec.Provider)), lf("subject_key", rec.SubjectKey), lf("table", rec.Table),
		lf("op", rec.Operation), lf("row_id", rec.RowID), lf("external_id", rec.ExternalID),
		lf("rows", strconv.Itoa(maxInt(rec.RowsAffected, 1))),
	}
	if rec.FindingID != uuid.Nil {
		fields = append(fields, lf("finding_id", rec.FindingID.String()))
	}
	if rec.FindingType != "" {
		fields = append(fields, lf("finding_type", string(rec.FindingType)))
	}
	if len(rec.Evidence) > 0 {
		keys := make([]string, 0, len(rec.Evidence))
		for key := range rec.Evidence {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fields = append(fields, lf("evidence."+key, fmt.Sprint(rec.Evidence[key])))
		}
	}
	writeLogLine(w, ts, fields...)
}

type logField struct{ key, value string }

func lf(key, value string) logField { return logField{key: key, value: value} }

func lfTime(key string, value time.Time) logField {
	if value.IsZero() {
		return lf(key, "")
	}
	return lf(key, value.UTC().Format(time.RFC3339))
}

func lfTimePtr(key string, value *time.Time) logField {
	if value == nil {
		return lf(key, "")
	}
	return lfTime(key, *value)
}

func writeLogLine(w io.Writer, ts time.Time, fields ...logField) {
	parts := []string{"ts=" + quoteLogValue(ts.UTC().Format(time.RFC3339))}
	for _, field := range fields {
		if field.key == "" || field.value == "" {
			continue
		}
		parts = append(parts, field.key+"="+quoteLogValue(field.value))
	}
	fmt.Fprintln(w, strings.Join(parts, " "))
}

func quoteLogValue(value string) string {
	if value == "" {
		return strconv.Quote(value)
	}
	if strings.ContainsAny(value, " \t\n\r\"=") {
		return strconv.Quote(value)
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
