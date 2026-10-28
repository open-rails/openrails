package hosttools

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/merchant"
	boot "github.com/open-rails/openrails/internal/merchantbootstrap"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
)

// PruneOptions mirrors `openrails prune`.
type PruneOptions struct {
	Config     *config.Config
	PGXPool    *pgxpool.Pool
	MerchantID billing.MerchantID
	// Rails narrows the pass to these rails; empty is every armed rail.
	Rails []string
	// PSP pins one PSP by id or key; an id also names an archived one.
	PSP string
	// Since and Until bound the transaction window. Payments prune only when
	// both are set and the provider listed that window completely.
	Since, Until string
	// ExpectRows is the typed confirmation and the only way a prune writes:
	// nil plans; set, it applies when it equals the rows the pass found.
	ExpectRows *int
	// Actor is recorded on the destructive run; empty is "unknown".
	Actor  string
	Format string
	LogDir string
	Out    io.Writer
	// MerchantManifest is the parsed MODE-1 merchant manifest; nil reads
	// MerchantManifestPath (or the conventional path).
	MerchantManifest     *boot.BillingConfig
	MerchantManifestPath string
}

type pruneTarget struct {
	Provider reconcile.Provider
	Binding  reconcile.PSPBinding
	Result   reconcile.PruneResult
	fetcher  reconcile.RailFetcher
}

type pruneConverge struct {
	RunID             *uuid.UUID `json:"run_id"`
	Findings          int        `json:"findings"`
	AutoFixed         int        `json:"auto_fixed"`
	ReconcileRequired int        `json:"reconcile_required"`
	RequiresReview    int        `json:"requires_review"`
}

// Prune soft-deletes a PSP's local subscriptions and payments that its
// provider no longer lists. It reads the provider and never writes it. An
// applied prune stamps every row with a destructive run, which
// `openrails undo-run` reverses.
func Prune(ctx context.Context, opts PruneOptions) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	since, err := parseProviderTime(opts.Since, "since")
	if err != nil {
		return err
	}
	until, err := parseProviderTime(opts.Until, "until")
	if err != nil {
		return err
	}
	rt, cleanup, err := newProviderRuntime(ctx, providerRuntimeOptions{Config: opts.Config, PGXPool: opts.PGXPool, MerchantID: opts.MerchantID,
		MerchantManifest: opts.MerchantManifest, MerchantManifestPath: opts.MerchantManifestPath})
	if err != nil {
		return err
	}
	defer cleanup()
	merchantID := opts.MerchantID
	ctx = merchant.WithID(ctx, merchantID)
	release, err := waitForMerchantPullLock(ctx, rt.DB.Pool(), merchantID)
	if err != nil {
		return err
	}
	defer release()
	apply := opts.ExpectRows != nil
	started := time.Now().UTC()
	return rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		targets, err := pruneTargets(ctx, rt, opts.PSP, opts.Rails)
		if err != nil {
			return err
		}
		// --expect-rows is one PSP's count: applied to several, it would
		// authorize the same number for each.
		if apply && len(targets) > 1 {
			return fmt.Errorf("refusing to apply a prune across %d PSPs at once: --expect-rows is a per-PSP count. Scope it with --psp and apply one at a time", len(targets))
		}
		wrote := false
		for i := range targets {
			t := &targets[i]
			t.Result, err = reconcile.PrunePSPExcess(ctx, rt.DB, t.fetcher, t.Provider, t.Binding, reconcile.PruneParams{
				Since: since, Until: until, Apply: apply, ExpectedRows: opts.ExpectRows, Actor: opts.Actor,
			})
			if err != nil {
				return fmt.Errorf("prune %s (%s): %w", t.Provider, t.Binding.AccountID, err)
			}
			wrote = wrote || t.Result.RunID != uuid.Nil
		}
		var converged *pruneConverge
		if wrote {
			res, err := converge.NewConvergeEngine(rt.DB).Converge(ctx, converge.Scope{Merchant: merchantID})
			if err != nil {
				return fmt.Errorf("post-prune converge: %w", err)
			}
			converged = &pruneConverge{RunID: res.RunID, Findings: res.Findings, AutoFixed: res.AutoFixed,
				ReconcileRequired: res.ReconcileRequired, RequiresReview: res.RequiresReview}
		}
		logPath, err := writePruneLog(opts.LogDir, started, merchantID, apply, since, until, targets, converged)
		if err != nil {
			return err
		}
		return renderPrune(opts.Out, opts.Format, logPath, merchantID, apply, targets, converged)
	})
}

// pruneTargets arms the PSPs a prune reads: the --psp one, else the PSP each
// armed rail (narrowed by --rail) armed from.
func pruneTargets(ctx context.Context, rt *providerRuntime, psp string, rails []string) ([]pruneTarget, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	selected := map[reconcile.Provider]bool{}
	for _, r := range rails {
		selected[reconcile.Provider(strings.ToLower(strings.TrimSpace(r)))] = true
	}
	builder := reconcile.MerchantFetcherBuilder{StripeClients: rt.StripeClients, Config: rt.Config, Merchants: rt.Merchants, DB: rt.DB}
	if strings.TrimSpace(psp) != "" {
		provider, binding, err := resolvePSPTarget(ctx, rt.DB, psp)
		if err != nil {
			return nil, err
		}
		for p := range selected {
			if p != provider {
				return nil, fmt.Errorf("--psp %s is a %s account, but --rail includes %s", psp, provider, p)
			}
		}
		armed, _, _ := builder.BuildPSP(ctx, mid, binding.ID)
		fetcher, ok := armed.Fetchers[provider]
		if !ok {
			return nil, fmt.Errorf("PSP %s (%s %s) did not arm: its configuration or credentials are unavailable", psp, provider, binding.AccountID)
		}
		return []pruneTarget{{Provider: provider, Binding: binding, fetcher: fetcher}}, nil
	}
	armed := builder.Build(ctx, mid)
	var out []pruneTarget
	for provider, fetcher := range armed.Fetchers {
		if len(selected) > 0 && !selected[provider] {
			continue
		}
		binding := armed.Coverage[provider].Binding
		if binding.ID == uuid.Nil {
			return nil, fmt.Errorf("rail %s armed without a resolved PSP: refusing an unattributed prune", provider)
		}
		out = append(out, pruneTarget{Provider: provider, Binding: binding, fetcher: fetcher})
	}
	if len(out) == 0 {
		return nil, errors.New("no armed PSP to prune for this merchant (check --rail and the merchant's credentials)")
	}
	slices.SortFunc(out, func(a, b pruneTarget) int { return cmp.Compare(a.Provider, b.Provider) })
	return out, nil
}

func pruneRows(r reconcile.PruneResult) int { return r.Subscriptions + r.Payments }

func writePruneLog(logDir string, started time.Time, merchantID billing.MerchantID, apply bool, since, until time.Time, targets []pruneTarget, converged *pruneConverge) (string, error) {
	if strings.TrimSpace(logDir) == "" {
		logDir = "."
	}
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return "", fmt.Errorf("create prune log dir: %w", err)
	}
	path := filepath.Join(logDir, fmt.Sprintf("prune-%s-%s.log", started.Format("20060102T150405Z"), uuid.NewString()[:8]))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) // #nosec G304 -- logDir is an operator CLI flag, not request input
	if err != nil {
		return "", fmt.Errorf("open prune log: %w", err)
	}
	defer func() { _ = f.Close() }()
	phase := "planned"
	if apply {
		phase = "applied"
	}
	now := time.Now().UTC()
	writeLogLine(f, now, lf("event", "prune"), lf("merchant_id", merchantID.String()), lf("phase", phase),
		lfTime("started_at", started), lfTime("window_starts_at", since), lfTime("window_ends_at", until))
	for _, t := range targets {
		r := t.Result
		psp := []logField{lf("provider", string(t.Provider)), lf("psp_id", t.Binding.ID.String())}
		var run logField
		if r.RunID != uuid.Nil {
			run = lf("run_id", r.RunID.String())
		}
		writeLogLine(f, now, append([]logField{lf("event", "psp"), lf("account_id", t.Binding.AccountID), run,
			lf("subscriptions", strconv.Itoa(r.Subscriptions)), lf("payments", strconv.Itoa(r.Payments)),
			lf("order_attempts", strconv.Itoa(r.OrderAttempts)), lf("product_access", strconv.Itoa(r.ProductAccess)),
			lf("subscriptions_skipped", strconv.Itoa(r.SubscriptionsSkipped)), lf("payments_skipped", strconv.Itoa(r.PaymentsSkipped))}, psp...)...)
		for _, rows := range []struct {
			table string
			ids   []uuid.UUID
		}{{"subscriptions", r.SubscriptionIDs}, {"payments", r.PaymentIDs}} {
			for _, id := range rows.ids {
				writeLogLine(f, now, append([]logField{lf("event", "soft_delete"), lf("phase", phase), run, lf("table", rows.table), lf("row_id", id.String())}, psp...)...)
			}
		}
		for _, s := range []struct {
			table, reason string
			ids           []uuid.UUID
		}{
			{"subscriptions", cmp.Or(r.SubscriptionSkipReason, "grant_ledger_entangled"), r.SkippedSubscriptionIDs},
			{"payments", cmp.Or(r.PaymentSkipReason, "protected_dependents"), r.SkippedPaymentIDs},
		} {
			for _, id := range s.ids {
				writeLogLine(f, now, append([]logField{lf("event", "prune_skip"), lf("table", s.table), lf("row_id", id.String()), lf("reason", s.reason)}, psp...)...)
			}
		}
	}
	if converged != nil {
		fields := []logField{lf("event", "converge"), lf("findings", strconv.Itoa(converged.Findings)),
			lf("auto_fixed", strconv.Itoa(converged.AutoFixed)), lf("reconcile_required", strconv.Itoa(converged.ReconcileRequired)),
			lf("requires_review", strconv.Itoa(converged.RequiresReview))}
		if converged.RunID != nil {
			fields = append(fields, lf("converge_run_id", converged.RunID.String()))
		}
		writeLogLine(f, now, fields...)
	}
	writeLogLine(f, now, lf("event", "summary"), lf("phase", phase), lf("prune", formatPruneCounts(summarizePrune(targets, apply))))
	return path, nil
}

func renderPrune(w io.Writer, format, logPath string, merchantID billing.MerchantID, apply bool, targets []pruneTarget, converged *pruneConverge) error {
	counts := summarizePrune(targets, apply)
	if format == "json" {
		runs := []uuid.UUID{}
		rows := 0
		for _, t := range targets {
			rows += pruneRows(t.Result)
			if t.Result.RunID != uuid.Nil {
				runs = append(runs, t.Result.RunID)
			}
		}
		return json.NewEncoder(w).Encode(map[string]any{
			"applied": apply, "rows": rows, "runs": runs, "prune": counts, "converge": converged, "log_file": logPath,
		})
	}
	label := "plan"
	if apply {
		label = "applied"
	}
	fmt.Fprintf(w, "prune %s: %s\n", label, formatPruneCounts(counts))
	for _, t := range targets {
		if t.Result.RunID != uuid.Nil {
			fmt.Fprintf(w, "  %s (%s): destructive run %s — reverse with `openrails undo-run --merchant %s --run %s`\n",
				t.Provider, t.Binding.AccountID, t.Result.RunID, merchantFlag(merchantID), t.Result.RunID)
		}
	}
	if !apply {
		var planned []pruneTarget
		for _, t := range targets {
			if pruneRows(t.Result) > 0 {
				planned = append(planned, t)
			}
		}
		switch {
		case len(planned) == 1 && len(targets) == 1:
			fmt.Fprintf(w, "  PLAN ONLY — nothing was written. To apply: re-run with --expect-rows %d\n", pruneRows(planned[0].Result))
		case len(planned) > 0:
			fmt.Fprintln(w, "  PLAN ONLY — nothing was written. Apply one PSP at a time:")
			for _, t := range planned {
				fmt.Fprintf(w, "    --psp %s --expect-rows %d   (%s %s)\n", t.Binding.ID, pruneRows(t.Result), t.Provider, t.Binding.AccountID)
			}
		}
	}
	if converged != nil {
		fmt.Fprintf(w, "converge: %d finding(s), %d auto-fixed, %d reconcile-required, %d requires-review\n",
			converged.Findings, converged.AutoFixed, converged.ReconcileRequired, converged.RequiresReview)
	}
	fmt.Fprintf(w, "log file: %s\n", logPath)
	return nil
}

type pruneCounts map[string]map[string]int

func summarizePrune(targets []pruneTarget, apply bool) pruneCounts {
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
	key := "would_prune"
	if apply {
		key = "soft_deleted"
	}
	for _, t := range targets {
		r := t.Result
		add("subscriptions", key, r.Subscriptions)
		add("subscriptions", "skipped", r.SubscriptionsSkipped)
		add("payments", key, r.Payments)
		add("payments", "skipped", r.PaymentsSkipped)
		add("order_attempts", key, r.OrderAttempts)
		add("product_access", key, r.ProductAccess)
	}
	return out
}

func formatPruneCounts(counts pruneCounts) string {
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	var parts []string
	for _, table := range tables {
		var values []string
		for _, key := range []string{"soft_deleted", "would_prune", "skipped"} {
			if n := counts[table][key]; n > 0 {
				values = append(values, fmt.Sprintf("%s=%d", key, n))
			}
		}
		if len(values) > 0 {
			parts = append(parts, table+" "+strings.Join(values, " "))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

type logField struct{ key, value string }

func lf(key, value string) logField { return logField{key: key, value: value} }

func lfTime(key string, value time.Time) logField {
	if value.IsZero() {
		return lf(key, "")
	}
	return lf(key, value.UTC().Format(time.RFC3339))
}

// writeLogLine writes one logfmt line; empty fields are left out.
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
	if value == "" || strings.ContainsAny(value, " \t\n\r\"=") {
		return strconv.Quote(value)
	}
	return value
}
