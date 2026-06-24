package reconcile

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// RenderRunJSON writes a run + findings as indented JSON (CLI --format json).
func RenderRunJSON(w io.Writer, run RunRecord, findings []FindingRecord) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Run      RunRecord       `json:"run"`
		Findings []FindingRecord `json:"findings"`
	}{run, findings})
}

// RenderRunTable writes a human-readable run report: run header, per-provider
// summary (incl. dunning forensics), and the findings table.
func RenderRunTable(w io.Writer, run RunRecord, findings []FindingRecord) error {
	fmt.Fprintf(w, "Reconcile run %s\n", run.ID)
	fmt.Fprintf(w, "  mode=%s status=%s providers=%s\n", run.Mode, run.Status, strings.Join(run.Providers, ","))
	fmt.Fprintf(w, "  started=%s", run.StartedAt.Format("2006-01-02 15:04:05 MST"))
	if run.FinishedAt != nil {
		fmt.Fprintf(w, " finished=%s", run.FinishedAt.Format("2006-01-02 15:04:05 MST"))
	}
	fmt.Fprintln(w)
	if run.WindowSince != nil || run.WindowUntil != nil {
		fmt.Fprintf(w, "  window:")
		if run.WindowSince != nil {
			fmt.Fprintf(w, " since=%s", run.WindowSince.Format("2006-01-02"))
		}
		if run.WindowUntil != nil {
			fmt.Fprintf(w, " until=%s", run.WindowUntil.Format("2006-01-02"))
		}
		fmt.Fprintln(w)
	}
	if run.Error != "" {
		fmt.Fprintf(w, "  ERROR: %s\n", run.Error)
	}

	if len(run.Summary) > 0 {
		var summary RunSummary
		if err := json.Unmarshal(run.Summary, &summary); err == nil {
			renderSummaryTable(w, &summary)
		}
	}

	if len(findings) == 0 {
		fmt.Fprintln(w, "\nNo findings.")
		return nil
	}

	sorted := make([]FindingRecord, len(findings))
	copy(sorted, findings)
	sort.SliceStable(sorted, func(i, j int) bool {
		if a, b := severityRank(sorted[i].Severity), severityRank(sorted[j].Severity); a != b {
			return a < b
		}
		if sorted[i].Type != sorted[j].Type {
			return sorted[i].Type < sorted[j].Type
		}
		return sorted[i].SubjectKey < sorted[j].SubjectKey
	})

	fmt.Fprintf(w, "\nFindings (%d):\n", len(sorted))
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tTYPE\tPROVIDER\tSTATUS\tSUBJECT\tLAST SEEN\tRECOMMENDED ACTION")
	for _, f := range sorted {
		action := f.RecommendedAction
		if len(action) > 96 {
			action = action[:93] + "..."
		}
		provider := string(f.Provider)
		if provider == "" {
			provider = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			strings.ToUpper(string(f.Severity)), f.Type, provider, f.Status,
			truncate(f.SubjectKey, 40), f.LastSeenAt.Format("2006-01-02 15:04:05"), action)
	}
	return tw.Flush()
}

func renderSummaryTable(w io.Writer, summary *RunSummary) {
	providers := make([]string, 0, len(summary.Providers))
	for name := range summary.Providers {
		providers = append(providers, name)
	}
	sort.Strings(providers)

	for _, name := range providers {
		rep := summary.Providers[name]
		fmt.Fprintf(w, "\nProvider %s:\n", name)
		if rep.Aborted {
			fmt.Fprintf(w, "  ABORTED: %s\n", rep.Error)
			continue
		}
		if rep.Error != "" {
			fmt.Fprintf(w, "  ERROR: %s\n", rep.Error)
		}
		fmt.Fprintf(w, "  remote: %d subscriptions, %d transactions, %d vault entries; local: %d subscriptions\n",
			rep.RemoteSubscriptions, rep.RemoteTransactions, rep.RemoteVaultEntries, rep.LocalSubscriptions)
		fmt.Fprintf(w, "  findings: %d new, %d updated, %d requires-review, %d auto-resolved, %d auto-fixed\n",
			rep.NewFindings, rep.UpdatedFindings, rep.AdminRequired, rep.AutoResolved, rep.AutoFixed)
		if len(rep.FindingsByType) > 0 {
			types := make([]string, 0, len(rep.FindingsByType))
			for t := range rep.FindingsByType {
				types = append(types, t)
			}
			sort.Strings(types)
			parts := make([]string, 0, len(types))
			for _, t := range types {
				parts = append(parts, fmt.Sprintf("%s=%d", t, rep.FindingsByType[t]))
			}
			fmt.Fprintf(w, "  by type: %s\n", strings.Join(parts, " "))
		}
		for _, applyErr := range rep.ApplyErrors {
			fmt.Fprintf(w, "  apply error: %s\n", applyErr)
		}
		if rep.Dunning != nil {
			renderForensics(w, rep.Dunning)
		}
	}

	if summary.StuckIntents != nil && summary.StuckIntents.Total > 0 {
		renderStuckIntents(w, summary.StuckIntents)
	}
}

// renderStuckIntents renders the provider-independent PS-10 section: count by
// intent type, then one line per stuck intent. Callers skip it entirely when
// nothing is stuck.
func renderStuckIntents(w io.Writer, rep *StuckIntentReport) {
	fmt.Fprintf(w, "\nStuck provider intents (%d): %d requires-review, %d mode-parked (informational), %d auto-resolved\n",
		rep.Total, rep.AdminRequired, rep.ModeParked, rep.AutoResolved)
	if len(rep.ByIntentType) > 0 {
		types := make([]string, 0, len(rep.ByIntentType))
		for t := range rep.ByIntentType {
			types = append(types, t)
		}
		sort.Strings(types)
		parts := make([]string, 0, len(types))
		for _, t := range types {
			parts = append(parts, fmt.Sprintf("%s=%d", t, rep.ByIntentType[t]))
		}
		fmt.Fprintf(w, "  by intent type: %s\n", strings.Join(parts, " "))
	}
	for _, line := range rep.Intents {
		note := ""
		if line.ModeParked {
			note = " [mode-parked: informational]"
		}
		fmt.Fprintf(w, "  %s %s/%s status=%s origin=%s attempts=%d age=%s%s\n",
			line.IntentID, line.Provider, line.IntentType, line.Status,
			line.Origin, line.Attempts, line.Age, note)
		if line.LastFailureReason != "" {
			fmt.Fprintf(w, "    last failure reason: %s\n", truncate(line.LastFailureReason, 120))
		}
	}
}

func renderForensics(w io.Writer, d *DunningForensics) {
	fmt.Fprintf(w, "  dunning forensics: %d examined — %d never-attempted, %d attempted+exhausted, %d in-progress, %d without remote declines\n",
		d.SubscriptionsExamined, d.NeverAttempted, d.AttemptedExhausted, d.AttemptedInProgress, d.NoRemoteDeclines)
	if d.HistorySource != "" {
		fmt.Fprintf(w, "    history source (analytics events): %s\n", d.HistorySource)
	}
	if d.LastLocalDunningAction != nil {
		fmt.Fprintf(w, "    local dunning last acted: %s\n", d.LastLocalDunningAction.Format("2006-01-02 15:04:05 MST"))
	} else if d.SubscriptionsExamined > 0 {
		fmt.Fprintf(w, "    local dunning last acted: NEVER (no last_retry_at on any examined subscription)\n")
	}
	if d.LastDunningActionAnySource != nil {
		fmt.Fprintf(w, "    last dunning action (ANY source): %s via %s\n",
			d.LastDunningActionAnySource.Format("2006-01-02 15:04:05 MST"), d.LastDunningActionVia)
	}
	if len(d.DeclineReasons) > 0 {
		type rc struct {
			reason string
			n      int
		}
		reasons := make([]rc, 0, len(d.DeclineReasons))
		for reason, n := range d.DeclineReasons {
			reasons = append(reasons, rc{reason, n})
		}
		sort.Slice(reasons, func(i, j int) bool {
			if reasons[i].n != reasons[j].n {
				return reasons[i].n > reasons[j].n
			}
			return reasons[i].reason < reasons[j].reason
		})
		fmt.Fprintf(w, "    decline reasons:\n")
		for _, r := range reasons {
			fmt.Fprintf(w, "      %4dx %s\n", r.n, truncate(r.reason, 80))
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
