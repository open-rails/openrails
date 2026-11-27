package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/metrics"
)

type rangeSpec struct{ Name, Last, Next, Grain string }

var readRanges = []rangeSpec{
	{"12m by month", "12m", "", "month"},
	{"90d by day", "90d", "", "day"},
	{"12w by week", "12w", "", "week"},
}

type readVariant struct {
	Measure string
	Name    string
	By      []string
	KPI     bool
	Arms    []string
}

var readVariants = []readVariant{
	{Measure: "mrr", Arms: []string{armFacts, armClose}},
	{Measure: "mrr", Name: "by product_id", By: []string{"product_id"}, Arms: []string{armFacts, armWide, armClose}},
	{Measure: "subscribers", Arms: []string{armFacts, armClose}},
	{Measure: "subscribers", Name: "by product_id", By: []string{"product_id"}, Arms: []string{armFacts, armWide, armClose}},
	{Measure: "arpa", Arms: []string{armFacts, armClose}},
	{Measure: "mrr_movements", Arms: []string{armFacts}},
	{Measure: "mrr_movements", Name: "by movement", By: []string{"movement"}, Arms: []string{armFacts}},
	{Measure: "subscriber_movements", Arms: []string{armFacts}},
	{Measure: "subscriber_movements", Name: "by movement", By: []string{"movement"}, Arms: []string{armFacts}},
	{Measure: "scheduled_mrr", Arms: []string{armFacts}},
	{Measure: "scheduled_mrr", Name: "by movement", By: []string{"movement"}, Arms: []string{armFacts}},
	{Measure: "subscriber_churn_rate", Arms: []string{armFacts, armClose}},
	{Measure: "subscriber_churn_rate", Name: "by churn_type", By: []string{"churn_type"}, Arms: []string{armFacts}},
	{Measure: "nrr", Arms: []string{armFacts, armClose}},
	{Measure: "grr", Arms: []string{armFacts, armClose}},
	{Measure: "cohort_retention", Arms: []string{armFacts}},
	{Measure: "ltv", Arms: []string{armFacts, armClose}},
	{Measure: "mrr_at_risk", Arms: []string{armFacts}},
	{Measure: "mrr_at_risk", Name: "by risk_reason", By: []string{"risk_reason"}, Arms: []string{armFacts}},
	{Measure: "at_risk_recovery_rate", Arms: []string{armFacts}},
	{Measure: "at_risk_recovery_rate", Name: "by risk_reason", By: []string{"risk_reason"}, Arms: []string{armFacts}},
}

// The tiles: a value at the range's end.
var kpiVariants = []readVariant{
	{Measure: "mrr", Name: "kpi", KPI: true, Arms: []string{armFacts, armClose}},
	{Measure: "subscribers", Name: "kpi", KPI: true, Arms: []string{armFacts, armClose}},
	{Measure: "arpa", Name: "kpi", KPI: true, Arms: []string{armFacts, armClose}},
	{Measure: "mrr_at_risk", Name: "kpi", KPI: true, Arms: []string{armFacts, armState}},
}

type armResult struct {
	Statements  int
	P50MS       float64
	MinMS       float64
	MaxMS       float64
	ExecMS      float64
	Buffers     int64
	ReadBlocks  int64
	RowsScanned int64
	RowsOut     int
	Err         string `json:",omitempty"`
	plan        string
	values      map[string]map[string]string
}

type readRow struct {
	Measure, Variant, Range, Zone string
	Arms                          map[string]*armResult
	Match                         map[string]string
	Diffs                         map[string][]string `json:",omitempty"`
	Err                           string              `json:",omitempty"`
}

type readJob struct {
	v    readVariant
	r    rangeSpec
	zone *time.Location
}

func readJobs() ([]readJob, error) {
	york, err := time.LoadLocation("America/New_York")
	if err != nil {
		return nil, err
	}
	var base []readJob
	for _, v := range readVariants {
		for _, r := range readRanges {
			if v.Measure == "scheduled_mrr" {
				r = rangeSpec{Name: strings.Replace(r.Name, r.Last, "next "+r.Last, 1), Next: r.Last, Grain: r.Grain}
			}
			base = append(base, readJob{v: v, r: r})
		}
	}
	for _, v := range kpiVariants {
		base = append(base, readJob{v: v, r: rangeSpec{Name: "kpi (last 12m)", Last: "12m"}})
	}
	var jobs []readJob
	for _, zone := range []*time.Location{time.UTC, york} {
		for _, j := range base {
			j.zone = zone
			jobs = append(jobs, j)
		}
	}
	return jobs, nil
}

// readStatements is a job's plan and its statements per arm: today's from
// the metrics compiler, the others from facts.go.
func readStatements(j readJob, mid uuid.UUID, asof time.Time) (map[string][]statement, map[string]string, error) {
	q := billing.QueryMetricsParams{Measures: []string{j.v.Measure}, Range: &billing.MetricsRange{Last: j.r.Last, Next: j.r.Next}}
	switch {
	case j.v.Measure == "cohort_retention":
		q.By = []string{"cohort", "months_since_start"}
	case !j.v.KPI:
		q.By, q.Grain = append([]string{"time"}, j.v.By...), j.r.Grain
	}
	plan, refused := metrics.ValidateIn(&q, asof, j.zone)
	if refused != nil {
		return nil, nil, fmt.Errorf("refused: %+v", refused.Errors)
	}
	compiled, err := metrics.Statements(plan, mid)
	if err != nil {
		return nil, nil, fmt.Errorf("compile: %w", err)
	}
	today := make([]statement, len(compiled))
	for i, st := range compiled {
		today[i] = statement{Name: string(st.Family), SQL: st.SQL, Args: st.Args}
	}
	arms, errs := map[string][]statement{"today": today}, map[string]string{}
	for _, arm := range j.v.Arms {
		st, err := factsStatements(plan, j.v.Measure, arm, mid, asof)
		if err != nil {
			errs[arm] = err.Error()
			continue
		}
		arms[arm] = st
	}
	return arms, errs, nil
}

func measureReads(ctx context.Context, pool *pgxpool.Pool, c config, w *world, asof time.Time, rep *report) error {
	conn, err := measureConn(ctx, pool)
	if err != nil {
		return err
	}
	defer conn.Release()
	jobs, err := readJobs()
	if err != nil {
		return err
	}
	plans := &strings.Builder{}
	for _, j := range jobs {
		row := &readRow{Measure: j.v.Measure, Variant: j.v.Name, Range: j.r.Name, Zone: j.zone.String(),
			Arms: map[string]*armResult{}, Match: map[string]string{}, Diffs: map[string][]string{}}
		rep.Reads = append(rep.Reads, row)
		arms, errs, err := readStatements(j, w.merchant, asof)
		if err != nil {
			row.Err = err.Error()
			continue
		}
		for arm, e := range errs {
			row.Arms[arm] = &armResult{Err: e}
		}
		for _, arm := range append([]string{"today"}, j.v.Arms...) {
			st, ok := arms[arm]
			if !ok {
				continue
			}
			res := timeStatements(ctx, conn.Conn(), st, c.runs)
			row.Arms[arm] = res
			fmt.Fprintf(plans, "\n===== %s %s | %s | %s | %s: %.1f ms p50, %d buffers\n%s", row.Measure, row.Variant, row.Range, row.Zone, arm, res.P50MS, res.Buffers, res.plan)
		}
		base := row.Arms["today"]
		for _, arm := range j.v.Arms {
			other := row.Arms[arm]
			if base == nil || other == nil || base.Err != "" || other.Err != "" {
				row.Match[arm] = "error"
				continue
			}
			row.Match[arm], row.Diffs[arm] = compareValues(base.values, other.values)
		}
		logf("%-22s %-15s %-16s %-17s today %8.1f ms %8d buf | %s", row.Measure, row.Variant, row.Range, row.Zone,
			base.P50MS, base.Buffers, armLine(row, j.v.Arms))
	}
	rep.Plans = plans.String()
	return nil
}

// dumpSQL writes every read's statements, without a database, for a parser
// to check.
func dumpSQL(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	jobs, err := readJobs()
	if err != nil {
		return err
	}
	asof := time.Now().UTC().Truncate(time.Second)
	n := 0
	for _, j := range jobs {
		arms, errs, err := readStatements(j, uuid.New(), asof)
		if err != nil {
			return fmt.Errorf("%s %s %s: %w", j.v.Measure, j.v.Name, j.r.Name, err)
		}
		if len(errs) > 0 {
			return fmt.Errorf("%s %s: %v", j.v.Measure, j.v.Name, errs)
		}
		for arm, sts := range arms {
			for _, st := range sts {
				n++
				name := fmt.Sprintf("%04d-%s-%s-%s.sql", n, j.v.Measure, arm, st.Name)
				if err := os.WriteFile(filepath.Join(dir, name), []byte(st.SQL+"\n"), 0o644); err != nil {
					return err
				}
			}
		}
	}
	for name, sql := range map[string]string{"sim.sql": simSQL, "settle_facts.sql": settleFactsSQL, "proto.sql": protoSQL, "proto_indexes.sql": protoIndexesSQL,
		"month_close.sql": monthCloseSQL, "product_close.sql": productCloseSQL, "customer_logos.sql": customerLogosSQL, "customer_close.sql": customerCloseSQL} {
		if err := os.WriteFile(filepath.Join(dir, "0000-"+name), []byte(sql), 0o644); err != nil {
			return err
		}
	}
	logf("wrote %d statements to %s", n, dir)
	return nil
}

func armLine(row *readRow, arms []string) string {
	var out []string
	for _, arm := range arms {
		r := row.Arms[arm]
		if r == nil {
			continue
		}
		if r.Err != "" {
			out = append(out, arm+" ERR "+firstLine(r.Err))
			continue
		}
		out = append(out, fmt.Sprintf("%s %8.1f ms %8d buf %s", arm, r.P50MS, r.Buffers, row.Match[arm]))
	}
	return strings.Join(out, " | ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

type explainNode struct {
	Type      string        `json:"Node Type"`
	Relation  string        `json:"Relation Name"`
	Index     string        `json:"Index Name"`
	Rows      float64       `json:"Actual Rows"`
	Loops     float64       `json:"Actual Loops"`
	SharedHit int64         `json:"Shared Hit Blocks"`
	SharedRd  int64         `json:"Shared Read Blocks"`
	Plans     []explainNode `json:"Plans"`
}

func (n explainNode) scanned() int64 {
	var total int64
	if n.Relation != "" {
		total += int64(n.Rows * n.Loops)
	}
	for _, c := range n.Plans {
		total += c.scanned()
	}
	return total
}

// timeStatements runs a read as the metrics service does, its statements one
// after another: one EXPLAIN (ANALYZE, BUFFERS) of each, a warm run whose
// rows are kept, then runs timed whole.
func timeStatements(ctx context.Context, conn pgxConn, stmts []statement, runs int) *armResult {
	r := &armResult{Statements: len(stmts)}
	plan := &strings.Builder{}
	for _, st := range stmts {
		var raw []byte
		if err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+st.SQL, st.Args...).Scan(&raw); err != nil {
			r.Err = st.Name + ": " + err.Error() + "\n" + st.SQL
			r.plan = r.Err
			return r
		}
		var parsed []struct {
			Plan      explainNode `json:"Plan"`
			Execution float64     `json:"Execution Time"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed) == 0 {
			r.Err = st.Name + ": explain: " + fmt.Sprint(err)
			return r
		}
		top := parsed[0]
		r.ExecMS += top.Execution
		r.Buffers += top.Plan.SharedHit + top.Plan.SharedRd
		r.ReadBlocks += top.Plan.SharedRd
		r.RowsScanned += top.Plan.scanned()
		rows, err := conn.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+st.SQL, st.Args...)
		if err == nil {
			fmt.Fprintf(plan, "-- %s\n", st.Name)
			for rows.Next() {
				var line string
				if rows.Scan(&line) == nil {
					plan.WriteString(line + "\n")
				}
			}
			rows.Close()
		}
	}
	r.plan = plan.String()
	values, n, err := collect(ctx, conn, stmts)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.values, r.RowsOut = values, n
	durs := make([]time.Duration, 0, runs)
	for range runs {
		t0 := time.Now()
		for _, st := range stmts {
			rows, err := conn.Query(ctx, st.SQL, st.Args...)
			if err != nil {
				r.Err = err.Error()
				return r
			}
			for rows.Next() {
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				r.Err = err.Error()
				return r
			}
		}
		durs = append(durs, time.Since(t0))
	}
	slices.Sort(durs)
	r.P50MS, r.MinMS, r.MaxMS = ms(durs[len(durs)/2]), ms(durs[0]), ms(durs[len(durs)-1])
	return r
}

// collect is each statement's leaves by bucket and dimensions.
func collect(ctx context.Context, conn pgxConn, stmts []statement) (map[string]map[string]string, int, error) {
	out := map[string]map[string]string{}
	n := 0
	for _, st := range stmts {
		rows, err := conn.Query(ctx, st.SQL, st.Args...)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", st.Name, err)
		}
		fields := rows.FieldDescriptions()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				rows.Close()
				return nil, 0, err
			}
			n++
			var key []string
			leaves := map[string]string{}
			for i, f := range fields {
				name := f.Name
				switch {
				case name == "bucket":
					key = append(key, "t="+cell(vals[i]))
				case strings.HasPrefix(name, "dim_"):
					key = append(key, name[4:]+"="+cell(vals[i]))
				case strings.HasPrefix(name, "m_"):
					leaves[name[2:]] = cell(vals[i])
				}
			}
			sort.Strings(key)
			k := strings.Join(key, " ")
			if out[k] == nil {
				out[k] = map[string]string{}
			}
			for l, v := range leaves {
				out[k][l] = v
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", st.Name, err)
		}
	}
	return out, n, nil
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return "0"
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case int64:
		return strconv.FormatInt(x, 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case pgtype.Numeric:
		b, _ := x.MarshalJSON()
		return strings.Trim(string(b), `"`)
	case [16]byte:
		return uuid.UUID(x).String()
	}
	return fmt.Sprint(v)
}

// compareValues compares the leaves both reads give, a missing cell as zero.
func compareValues(a, b map[string]map[string]string) (string, []string) {
	leavesOf := func(m map[string]map[string]string) map[string]bool {
		out := map[string]bool{}
		for _, row := range m {
			for l := range row {
				out[l] = true
			}
		}
		return out
	}
	la, lb := leavesOf(a), leavesOf(b)
	var common, missing []string
	for l := range la {
		if lb[l] {
			common = append(common, l)
		} else {
			missing = append(missing, l)
		}
	}
	sort.Strings(common)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	cells, diffs := 0, 0
	var samples []string
	get := func(m map[string]map[string]string, k, l string) string {
		if v, ok := m[k][l]; ok {
			return v
		}
		return "0"
	}
	for _, k := range sorted {
		for _, l := range common {
			cells++
			va, vb := get(a, k, l), get(b, k, l)
			if va != vb {
				diffs++
				if len(samples) < 8 {
					samples = append(samples, fmt.Sprintf("%s %s: today %s, other %s", k, l, va, vb))
				}
			}
		}
	}
	status := "="
	if diffs > 0 {
		status = fmt.Sprintf("≠ %d/%d", diffs, cells)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		status += " (no " + strings.Join(missing, ", ") + ")"
	}
	if len(common) == 0 {
		status = "no common leaves"
	}
	return status, samples
}
