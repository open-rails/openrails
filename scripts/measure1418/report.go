package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type tableRow struct {
	Table, Kind string
	Rows, Bytes int64
}

type report struct {
	c               config
	Customers       int
	Version         string
	Sim             simSummary
	FutureMovements int64
	RiskCases       int64
	RiskCasesMoved  int64
	Tables          []tableRow
	Reads           []*readRow
	Writes          []writeRow
	Plans           string `json:"-"`
}

func newReport(c config) *report { return &report{c: c, Customers: c.customers} }

var tableKinds = []struct{ kind, name string }{
	{"fact", "billing.mrr_movements"}, {"fact", "billing.mrr_movement_lines"}, {"fact", "billing.subscription_mrr"},
	{"fact", "billing.customer_mrr"}, {"fact", "billing.mrr_scheduled"}, {"fact", "billing.mrr_risk_cases"},
	{"fact", "billing.mrr_risk_episodes"}, {"fact", "billing.subscription_cancellations"},
	{"rollup", "billing.mrr_flows"}, {"rollup", "billing.mrr_totals"}, {"rollup", "billing.mrr_product_flows"},
	{"rollup", "billing.mrr_product_totals"}, {"rollup", "billing.mrr_cohort_flows"}, {"rollup", "billing.mrr_streak_flows"},
	{"rollup", "billing.mrr_customer_flows"}, {"rollup", "billing.mrr_customer_totals"}, {"rollup", "billing.mrr_risk_flows"},
	{"rollup", "billing.mrr_risk_totals"}, {"queue", "billing.mrr_risk_due"},
	{"other", "billing.subscriptions"}, {"other", "billing.customers"}, {"other", "billing.subscription_status_transitions"},
	{"proposed", "proto.lines_wide"}, {"proposed", "proto.mrr_month_close"},
}

var protoIndexes = []string{"proto_mrr_movements_at_idx", "proto_mrr_movements_cohort_idx", "proto_mrr_movements_logo_idx",
	"proto_mrr_risk_cases_opened_idx", "proto_mrr_risk_cases_closed_idx", "proto_mrr_risk_episodes_entered_idx", "proto_mrr_risk_episodes_left_idx"}

func (r *report) tables(ctx context.Context, pool *pgxpool.Pool, mid uuid.UUID) error {
	for _, t := range tableKinds {
		var row tableRow
		row.Table, row.Kind = t.name, t.kind
		if err := pool.QueryRow(ctx, "SELECT count(*), pg_total_relation_size('"+t.name+"') FROM "+t.name).Scan(&row.Rows, &row.Bytes); err != nil {
			return fmt.Errorf("%s: %w", t.name, err)
		}
		r.Tables = append(r.Tables, row)
	}
	// At-risk cases whose subscription's MRR moved while they were open: the
	// amount a case's mrr_at_entry does not follow.
	var moved, cases int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE EXISTS (
		    SELECT 1 FROM billing.mrr_movement_lines l
		    JOIN billing.mrr_movements v ON v.merchant_id = l.merchant_id AND v.id = l.movement_id
		   WHERE l.merchant_id = c.merchant_id AND l.subscription_id = c.subscription_id
		     AND v.occurred_at > c.opened_at AND v.occurred_at < COALESCE(c.closed_at, 'infinity'))), count(*)
		FROM billing.mrr_risk_cases c WHERE c.merchant_id = $1`,
		mid).Scan(&moved, &cases); err != nil {
		return fmt.Errorf("risk diagnostic: %w", err)
	}
	r.RiskCasesMoved, r.RiskCases = moved, cases
	for _, ix := range protoIndexes {
		row := tableRow{Table: "index " + ix, Kind: "proposed"}
		if err := pool.QueryRow(ctx, "SELECT pg_relation_size(c.oid) FROM pg_class c WHERE c.relname = $1", ix).Scan(&row.Bytes); err != nil {
			return fmt.Errorf("%s: %w", ix, err)
		}
		r.Tables = append(r.Tables, row)
	}
	return nil
}

func (r *report) flush() {
	if b, err := json.MarshalIndent(r, "", "  "); err == nil {
		_ = os.WriteFile(outPath(r.c, "results.json"), b, 0o644)
	}
	_ = os.WriteFile(outPath(r.c, "summary.md"), []byte(r.markdown()), 0o644)
	_ = os.WriteFile(outPath(r.c, "plans.txt"), []byte(r.Plans), 0o644)
}

func (r *report) print() { fmt.Println(r.markdown()) }

func mb(b int64) string { return fmt.Sprintf("%.1f MB", float64(b)/1e6) }

func (r *report) markdown() string {
	b := &strings.Builder{}
	s := r.Sim
	fmt.Fprintf(b, "# MRR from rollups vs from facts: %d customers\n\n%s\n\n", r.Customers, r.Version)
	fmt.Fprintf(b, "History %s to %s (stopped early: %v): %d customers created, %d live at the end, %d reactivations; %d statements (%d failed) in %.0f s, %.0f/s. Pending scheduled churn written as future-dated movements: %d.\n\n",
		s.From.Format("2006-01-02"), s.To.Format("2006-01-02 15:04"), s.StoppedEarly, s.Customers, s.Live, s.Reactivations, s.Statements, s.Failed, s.Seconds, s.PerSecond, r.FutureMovements)
	if len(s.Errors) > 0 {
		b.WriteString("Generator errors (first):\n\n")
		for _, e := range s.Errors {
			fmt.Fprintf(b, "- `%s`\n", strings.ReplaceAll(firstLine(e), "`", "'"))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "At-risk cases: %d; with the subscription's MRR changed while open: %d.\n\n", r.RiskCases, r.RiskCasesMoved)
	b.WriteString("## Data\n\n| table | kind | rows | size |\n|---|---|---:|---:|\n")
	for _, t := range r.Tables {
		fmt.Fprintf(b, "| %s | %s | %d | %s |\n", t.Table, t.Kind, t.Rows, mb(t.Bytes))
	}
	b.WriteString("\n## Generation: each change through today's triggers (ms, one transaction)\n\n| change | n | failed | mean | p50 | p95 |\n|---|---:|---:|---:|---:|---:|\n")
	for _, k := range s.Kinds {
		fmt.Fprintf(b, "| %s | %d | %d | %.2f | %.2f | %.2f |\n", k.Kind, k.N, k.Failed, k.MeanMS, k.P50MS, k.P95MS)
	}
	b.WriteString("\n## Reads: p50 ms of the statements a query runs, shared buffers hit+read, rows scanned; = when the results agree\n\n")
	b.WriteString("| measure | variant | range | zone | today ms | today buf | today rows | facts ms | facts buf | facts rows | match | other arm |\n|---|---|---|---|---:|---:|---:|---:|---:|---:|---|---|\n")
	for _, row := range r.Reads {
		if row.Err != "" {
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |||||||\n", row.Measure, row.Variant, row.Range, row.Zone, strings.ReplaceAll(firstLine(row.Err), "|", "/"))
			continue
		}
		t, f := row.Arms["today"], row.Arms[armFacts]
		cellsOf := func(a *armResult) string {
			if a == nil {
				return " | | "
			}
			if a.Err != "" {
				return "ERR | | "
			}
			return fmt.Sprintf("%.1f | %d | %d", a.P50MS, a.Buffers, a.RowsScanned)
		}
		other := ""
		for _, arm := range []string{armWide, armClose, armState} {
			if a := row.Arms[arm]; a != nil {
				if a.Err != "" {
					other += arm + " ERR "
				} else {
					other += fmt.Sprintf("%s %.1f ms %d buf %s ", arm, a.P50MS, a.Buffers, row.Match[arm])
				}
			}
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", row.Measure, row.Variant, row.Range, row.Zone, cellsOf(t), cellsOf(f), row.Match[armFacts], other)
	}
	var mismatches []string
	for _, row := range r.Reads {
		for arm, d := range row.Diffs {
			if len(d) > 0 {
				mismatches = append(mismatches, fmt.Sprintf("- %s %s %s %s (%s): %s", row.Measure, row.Variant, row.Range, row.Zone, arm, strings.Join(d, "; ")))
			}
		}
		for arm, a := range row.Arms {
			if a.Err != "" {
				mismatches = append(mismatches, fmt.Sprintf("- %s %s %s %s (%s) ERROR: %s", row.Measure, row.Variant, row.Range, row.Zone, arm, strings.ReplaceAll(a.Err, "\n", " ")))
			}
		}
	}
	if len(mismatches) > 0 {
		b.WriteString("\n### Differences and errors\n\n" + strings.Join(mismatches, "\n") + "\n")
	}
	b.WriteString("\n## Writes: one subscription change, its transaction and commit (ms)\n\n| change | arm | n | failed | p50 | p95 | mean |\n|---|---|---:|---:|---:|---:|---:|\n")
	for _, w := range r.Writes {
		fmt.Fprintf(b, "| %s | %s | %d | %d | %.2f | %.2f | %.2f |\n", w.Change, w.Arm, w.N, w.Failed, w.P50MS, w.P95MS, w.MeanMS)
		for _, e := range w.Errors {
			fmt.Fprintf(b, "|  | error: %s ||||||\n", strings.ReplaceAll(firstLine(e), "|", "/"))
		}
	}
	type slow struct {
		row *readRow
		arm string
		a   *armResult
	}
	var slows []slow
	for _, row := range r.Reads {
		for arm, a := range row.Arms {
			if a.Err == "" {
				slows = append(slows, slow{row, arm, a})
			}
		}
	}
	sort.Slice(slows, func(i, j int) bool { return slows[i].a.P50MS > slows[j].a.P50MS })
	b.WriteString("\n## Slowest reads\n\n")
	for i, x := range slows {
		if i == 12 {
			break
		}
		fmt.Fprintf(b, "### %s %s, %s, %s: %s %.1f ms, %d buffers, %d rows scanned\n\n```\n%s```\n\n", x.row.Measure, x.row.Variant, x.row.Range, x.row.Zone, x.arm, x.a.P50MS, x.a.Buffers, x.a.RowsScanned, clipLines(x.a.plan, 60))
	}
	return b.String()
}

func clipLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append(lines[:n], "...")
	}
	return strings.Join(lines, "\n")
}
