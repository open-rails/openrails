package main

import (
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

	"github.com/open-rails/openrails/internal/reconcile"
)

type pullProviderMutationFlags struct {
	Insert    bool
	Overwrite bool
	Prune     bool
}

type pullProviderPruneLog struct {
	Provider reconcile.Provider
	Binding  reconcile.ProviderAccountBinding
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
		lf("event", "run"),
		lf("run_id", run.ID.String()),
		lf("status", run.Status),
		lf("mode", string(run.Mode)),
		lf("providers", strings.Join(run.Providers, ",")),
		lf("insert", strconv.FormatBool(flags.Insert)),
		lf("overwrite", strconv.FormatBool(flags.Overwrite)),
		lf("prune", strconv.FormatBool(flags.Prune)),
		lfTime("started_at", run.StartedAt),
		lfTimePtr("finished_at", run.FinishedAt),
		lfTimePtr("window_since", run.WindowSince),
		lfTimePtr("window_until", run.WindowUntil),
	)

	for _, rec := range res.Findings {
		writeLogLine(f, now,
			lf("event", "finding"),
			lf("run_id", run.ID.String()),
			lf("finding_id", rec.ID.String()),
			lf("provider", string(rec.Provider)),
			lf("type", string(rec.Type)),
			lf("status", string(rec.Status)),
			lf("severity", string(rec.Severity)),
			lf("subject_key", rec.SubjectKey),
			lfTime("last_seen_at", rec.LastSeenAt),
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
			writeLogLine(f, now,
				lf("event", "prune_skip"),
				lf("run_id", run.ID.String()),
				lf("provider", string(pl.Provider)),
				lf("provider_account_id", pl.Binding.ID.String()),
				lf("table", "subscriptions"),
				lf("row_id", id.String()),
				lf("reason", subReason),
			)
		}
		payReason := pl.Result.PaymentSkipReason
		if payReason == "" {
			payReason = "protected_dependents"
		}
		for _, id := range pl.Result.SkippedPaymentIDs {
			writeLogLine(f, now,
				lf("event", "prune_skip"),
				lf("run_id", run.ID.String()),
				lf("provider", string(pl.Provider)),
				lf("provider_account_id", pl.Binding.ID.String()),
				lf("table", "payments"),
				lf("row_id", id.String()),
				lf("reason", payReason),
			)
		}
	}
	if convergeLog != nil {
		fields := []logField{
			lf("event", "converge"),
			lf("run_id", run.ID.String()),
			lf("findings", strconv.Itoa(convergeLog.Findings)),
			lf("auto_fixed", strconv.Itoa(convergeLog.AutoFixed)),
			lf("reconcile_required", strconv.Itoa(convergeLog.ReconcileRequired)),
			lf("requires_review", strconv.Itoa(convergeLog.RequiresReview)),
		}
		if convergeLog.RunID != nil {
			fields = append(fields, lf("converge_run_id", convergeLog.RunID.String()))
		}
		writeLogLine(f, now, fields...)
	}

	counts := summarizeMutations(res.PlannedChanges, appliedChanges)
	writeLogLine(f, now,
		lf("event", "summary"),
		lf("run_id", run.ID.String()),
		lf("findings", strconv.Itoa(len(res.Findings))),
		lf("planned", formatMutationCounts(counts["planned"])),
		lf("applied", formatMutationCounts(counts["applied"])),
	)
	return path, nil
}

func pruneMutationRecords(provider reconcile.Provider, pr reconcile.PruneResult, phase string) []reconcile.MutationRecord {
	var out []reconcile.MutationRecord
	for _, id := range pr.SubscriptionIDs {
		out = append(out, reconcile.MutationRecord{
			Phase:        phase,
			Provider:     provider,
			Table:        "subscriptions",
			Operation:    "delete",
			RowID:        id.String(),
			RowsAffected: 1,
		})
	}
	for _, id := range pr.PaymentIDs {
		out = append(out, reconcile.MutationRecord{
			Phase:        phase,
			Provider:     provider,
			Table:        "payments",
			Operation:    "delete",
			RowID:        id.String(),
			RowsAffected: 1,
		})
	}
	return out
}

func renderPullProviderStdout(w io.Writer, format, logPath string, run reconcile.RunRecord, res *reconcile.RunResult, appliedChanges []reconcile.MutationRecord, pruneLogs []pullProviderPruneLog, convergeLog *pullProviderConvergeLog) error {
	counts := summarizeMutations(res.PlannedChanges, appliedChanges)
	statusCounts := findingStatusCounts(res.Findings)
	pruneCounts := summarizePrune(pruneLogs)
	if format == "json" {
		return json.NewEncoder(w).Encode(map[string]any{
			"run_id":          run.ID,
			"status":          run.Status,
			"log_file":        logPath,
			"findings":        statusCounts,
			"planned_changes": counts["planned"],
			"applied_changes": counts["applied"],
			"prune":           pruneCounts,
			"converge":        convergeLog,
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
		fmt.Fprintf(w, "converge: %d finding(s), %d auto-fixed, %d reconcile-required, %d requires-review\n",
			convergeLog.Findings, convergeLog.AutoFixed, convergeLog.ReconcileRequired, convergeLog.RequiresReview)
	}
	fmt.Fprintf(w, "log file: %s\n", logPath)
	return nil
}

type pruneCounts map[string]map[string]int

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

type mutationCounts map[string]map[string]int

func summarizeMutations(planned, applied []reconcile.MutationRecord) map[string]mutationCounts {
	out := map[string]mutationCounts{
		"planned": {},
		"applied": {},
	}
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

func formatMutationCounts(counts mutationCounts) string {
	if len(counts) == 0 {
		return "none"
	}
	tables := make([]string, 0, len(counts))
	for table := range counts {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	ops := []string{"insert", "update", "delete"}
	var tableParts []string
	for _, table := range tables {
		var opParts []string
		for _, op := range ops {
			if n := counts[table][op]; n > 0 {
				opParts = append(opParts, fmt.Sprintf("%s=%d", pastTense(op), n))
			}
		}
		if len(opParts) == 0 {
			continue
		}
		tableParts = append(tableParts, fmt.Sprintf("%s %s", table, strings.Join(opParts, " ")))
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
		lf("event", "mutation"),
		lf("run_id", runID.String()),
		lf("phase", rec.Phase),
		lf("provider", string(rec.Provider)),
		lf("subject_key", rec.SubjectKey),
		lf("table", rec.Table),
		lf("op", rec.Operation),
		lf("row_id", rec.RowID),
		lf("external_id", rec.ExternalID),
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

type logField struct {
	key   string
	value string
}

func lf(key, value string) logField {
	return logField{key: key, value: value}
}

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
