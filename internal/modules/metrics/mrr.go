package metrics

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The MRR families read the facts: movements, their lines, and the at-risk
// cases and episodes, each by an index on the merchant and an instant, every
// read bounded above by the plan's as-of instant. One statement per family.

// binder numbers a statement's arguments.
type binder struct{ args []any }

func (b *binder) arg(v any) string {
	b.args = append(b.args, v)
	return fmt.Sprintf("$%d", len(b.args))
}

// churnTypeExpr is a churn movement's churn_type.
const churnTypeExpr = `CASE WHEN mv.voluntary THEN 'voluntary' WHEN mv.cause = 'chargeback' THEN 'chargeback' ELSE 'involuntary' END`

// churnTypes joins every churn_type to each row (each type sees every
// subscriber), restricted to a churn_type filter.
func churnTypes(plan *Plan, b *binder) (join string, where []string) {
	if !slices.Contains(plan.Dims, "churn_type") && plan.Filters["churn_type"] == nil {
		return "", nil
	}
	join = "\nCROSS JOIN (VALUES ('voluntary'), ('involuntary'), ('chargeback')) ct(churn_type)"
	if f := plan.Filters["churn_type"]; f != nil {
		where = append(where, "ct.churn_type = ANY("+b.arg(f)+")")
	}
	return join, where
}

// snapshotSource is the rows a snapshot sums before each edge.
type snapshotSource struct {
	from, merchant, at string
	dims               map[string]string
}

// snapshotSQL sums each leaf's expression over src's rows dated before each
// edge: one range scan up to the last edge (never past as-of), its rows
// numbered by the edges they precede (width_bucket) and summed per edge, so
// the work is the rows read, not edges times rows. Dimensions of the source
// group the rows; churn_type repeats every row for each type.
func snapshotSQL(plan *Plan, b *binder, merchant string, src snapshotSource, leaves []*Measure, exprOf func(*Measure) string, edges []time.Time) (string, error) {
	e := b.arg(edges)
	where := []string{src.merchant + " = " + merchant, src.at + " < " + b.arg(plan.asOf(edges[len(edges)-1]))}
	for _, d := range sortedFilterDims(plan.Filters) {
		if d == "churn_type" {
			continue
		}
		expr, ok := src.dims[d]
		if !ok {
			return "", fmt.Errorf("metrics: snapshot lacks filter dimension %s (registry bug)", d)
		}
		where = append(where, expr+" = ANY("+b.arg(plan.Filters[d])+")")
	}
	var gsel, gby, dsel, on, osel, oby []string
	for _, d := range plan.Dims {
		if d == "churn_type" {
			osel = append(osel, "ct.churn_type AS dim_churn_type")
			oby = append(oby, "ct.churn_type")
			continue
		}
		expr, ok := src.dims[d]
		if !ok {
			return "", fmt.Errorf("metrics: snapshot lacks dimension %s (registry bug)", d)
		}
		n := len(dsel)
		gsel = append(gsel, fmt.Sprintf("%s AS d%d", expr, n))
		gby = append(gby, fmt.Sprint(n+1))
		dsel = append(dsel, fmt.Sprintf("d%d", n))
		on = append(on, fmt.Sprintf("g.d%d = d.d%d", n, n))
		osel = append(osel, fmt.Sprintf("d.d%d AS dim_%s", n, d))
		oby = append(oby, fmt.Sprintf("d.d%d", n))
	}
	gsel = append(gsel, "width_bucket("+src.at+", "+e+"::timestamptz[]) AS k")
	gby = append(gby, fmt.Sprint(len(dsel)+1))
	for i, m := range leaves {
		gsel = append(gsel, fmt.Sprintf("sum(%s) AS l%d", exprOf(m), i))
		osel = append(osel, fmt.Sprintf("COALESCE(sum(g.l%d), 0)::bigint AS m_%s", i, m.Name))
	}
	d := "(SELECT 1 AS one) d"
	if len(dsel) > 0 {
		d = "(SELECT DISTINCT " + strings.Join(dsel, ", ") + " FROM g) d"
	}
	join, ctWhere := churnTypes(plan, b)
	on = append(on, "g.k < e.k")
	sql := "WITH g AS (\n  SELECT " + strings.Join(gsel, ", ") +
		"\n  FROM " + src.from + "\n  WHERE " + strings.Join(where, " AND ") +
		"\n  GROUP BY " + strings.Join(gby, ", ") + "\n)" +
		"\nSELECT " + strings.Join(append([]string{"e.edge AS bucket"}, osel...), ", ") +
		"\nFROM unnest(" + e + "::timestamptz[]) WITH ORDINALITY AS e(edge, k)\nCROSS JOIN " + d + join +
		"\nLEFT JOIN g ON " + strings.Join(on, " AND ")
	if len(ctWhere) > 0 {
		sql += "\nWHERE " + strings.Join(ctWhere, " AND ")
	}
	return sql + "\nGROUP BY " + strings.Join(append([]string{"e.edge"}, oby...), ", "), nil
}

var (
	movementsSource = snapshotSource{from: "billing.mrr_movements m", merchant: "m.merchant_id", at: "m.occurred_at",
		dims: map[string]string{"currency": "m.currency"}}
	linesSource = snapshotSource{from: "billing.mrr_movement_lines l", merchant: "l.merchant_id", at: "l.occurred_at",
		dims: map[string]string{"currency": "l.currency", "product_id": "'prod_' || l.product_id::text"}}
)

// compileMRR is MRR, its customers and their values at a period's start: the
// movements before each edge; by product the lines; without a currency a
// count of customers counts each once.
func compileMRR(plan *Plan, merchantID uuid.UUID, leaves []*Measure) ([]stmt, error) {
	edges := snapshotEdges(plan, leaves)
	one := func(src snapshotSource, ls []*Measure, exprOf func(*Measure) string) (stmt, error) {
		b := &binder{}
		sql, err := snapshotSQL(plan, b, b.arg(merchantID), src, ls, exprOf, edges)
		return stmt{family: FamMRR, sql: sql, args: b.args, leaves: ls, hasTime: true, snapshot: true}, err
	}
	if byProduct(plan) {
		st, err := one(linesSource, leaves, func(m *Measure) string { return m.ByProduct })
		return []stmt{st}, err
	}
	perCurrency, distinct := leaves, []*Measure(nil)
	if !byCurrency(plan) {
		perCurrency = nil
		for _, m := range leaves {
			if m.ByCustomer != "" {
				distinct = append(distinct, m)
			} else {
				perCurrency = append(perCurrency, m)
			}
		}
	}
	var out []stmt
	if len(distinct) > 0 {
		st, err := one(movementsSource, distinct, func(m *Measure) string { return m.ByCustomer })
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	if len(perCurrency) > 0 {
		st, err := one(movementsSource, perCurrency, func(m *Measure) string { return m.Expr })
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// compileMRRBase is each period's opening base: the movements of the
// customers whose run of MRR began before the period, a movement being the
// base's unless its customer gained MRR from none earlier in the period. A
// period is a bucket, or the range without time; the read starts at the
// first bucket's start so a gain there is seen.
func compileMRRBase(plan *Plan, merchantID uuid.UUID, leaves []*Measure) (stmt, error) {
	b := &binder{}
	m := b.arg(merchantID)
	period, partition, start := "", "m.customer_id, m.currency", plan.From
	if plan.HasTime {
		period = bucketExpr(families[FamMRRBase], b.arg(plan.Grain), b.arg(plan.zone().String()))
		partition, start = partition+", "+period, plan.Buckets[0]
	}
	where := []string{"m.merchant_id = " + m, "m.occurred_at >= " + b.arg(start), "m.occurred_at < " + b.arg(plan.asOf(plan.To))}
	if f := plan.Filters["currency"]; f != nil {
		where = append(where, "m.currency = ANY("+b.arg(f)+")")
	}
	from := b.arg(plan.From)
	join, ctWhere := churnTypes(plan, b)
	sel, group := []string{}, []string{}
	if plan.HasTime {
		sel, group = append(sel, "mv.b AS bucket"), append(group, "mv.b")
	}
	for _, d := range plan.Dims {
		expr, ok := families[FamMRRBase].DimExprs[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: %s lacks dimension %s (registry bug)", FamMRRBase, d)
		}
		sel, group = append(sel, expr+" AS dim_"+d), append(group, expr)
	}
	typed := ""
	if join != "" {
		typed = "AND " + churnTypeExpr + " = ct.churn_type"
	}
	for _, l := range leaves {
		sel = append(sel, strings.ReplaceAll(l.Expr, "@churn_type", typed)+" AS m_"+l.Name)
	}
	bucket := ""
	if plan.HasTime {
		bucket = period + " AS b, "
	}
	sql := `WITH mv AS (
  SELECT m.currency, m.kind, m.delta, m.logo_delta, m.voluntary, m.cause, ` + bucket + `m.occurred_at >= ` + from + ` AS inr,
         bool_or(m.logo_delta = 1) OVER (PARTITION BY ` + partition + `
           ORDER BY m.occurred_at, m.id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS fresh
  FROM billing.mrr_movements m
  WHERE ` + strings.Join(where, " AND ") + `
)
SELECT ` + strings.Join(sel, ",\n       ") + `
FROM mv` + join + `
WHERE ` + strings.Join(append([]string{"mv.inr", "NOT mv.fresh"}, ctWhere...), " AND ")
	if len(group) > 0 {
		sql += "\nGROUP BY " + strings.Join(group, ", ")
	}
	return stmt{family: FamMRRBase, sql: sql, args: b.args, leaves: leaves, hasTime: plan.HasTime}, nil
}

// compileTrailing is LTV's churn: per bucket (the range's end without
// time), the three months ending at its end. The base is the customers with
// MRR at the window's start less those a correction took out in it, the
// churn the base's: an event counts when its run began before the window
// (its customer's latest gain from none within the rows read, none read
// being before them all).
func compileTrailing(plan *Plan, merchantID uuid.UUID, leaves []*Measure) (stmt, error) {
	labels := []time.Time{plan.To}
	if plan.HasTime {
		labels = plan.Buckets
	}
	starts, ends := make([]time.Time, len(labels)), make([]time.Time, len(labels))
	for i, l := range labels {
		starts[i], ends[i] = trailingWindow(plan, l)
	}
	uniq := slices.Clone(starts)
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Before(uniq[j]) })
	uniq = slices.CompactFunc(uniq, func(x, y time.Time) bool { return x.Equal(y) })
	hi := ends[0]
	for _, e := range ends {
		if e.After(hi) {
			hi = e
		}
	}
	b := &binder{}
	m := b.arg(merchantID)
	currency := ""
	if f := plan.Filters["currency"]; f != nil {
		currency = " AND m.currency = ANY(" + b.arg(f) + ")"
	}
	u := b.arg(uniq)
	sel := []string{"w.bucket AS bucket"}
	group := []string{"w.bucket", "s.currency", "s.subs"}
	for _, d := range plan.Dims {
		sel = append(sel, families[FamMRRTrailing].DimExprs[d]+" AS dim_"+d)
	}
	for _, l := range leaves {
		sel = append(sel, l.Expr+" AS m_"+l.Name)
	}
	sql := `WITH mv AS (
  SELECT m.currency, m.kind, m.logo_delta, m.occurred_at,
         max(m.occurred_at) FILTER (WHERE m.logo_delta = 1) OVER (PARTITION BY m.customer_id, m.currency ORDER BY m.occurred_at, m.id
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS began
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.logo_delta <> 0 AND m.occurred_at >= ` + b.arg(uniq[0]) + ` AND m.occurred_at < ` + b.arg(plan.asOf(hi)) + currency + `
), ev AS (
  SELECT mv.currency, mv.kind, mv.logo_delta, mv.occurred_at, mv.began FROM mv WHERE mv.kind IN ('churn', 'correction')
), g AS (
  SELECT m.currency, width_bucket(m.occurred_at, ` + u + `::timestamptz[]) AS k, sum(m.logo_delta) AS d
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.logo_delta <> 0 AND m.occurred_at < ` + b.arg(plan.asOf(uniq[len(uniq)-1])) + currency + `
  GROUP BY 1, 2
), s AS (
  SELECT c.currency, uu.u, COALESCE(sum(g.d), 0) AS subs
  FROM unnest(` + u + `::timestamptz[]) WITH ORDINALITY AS uu(u, k)
  CROSS JOIN (SELECT DISTINCT currency FROM g) c
  LEFT JOIN g ON g.currency = c.currency AND g.k < uu.k
  GROUP BY 1, 2
)
SELECT ` + strings.Join(sel, ",\n       ") + `
FROM unnest(` + b.arg(labels) + `::timestamptz[], ` + b.arg(starts) + `::timestamptz[], ` + b.arg(ends) + `::timestamptz[]) AS w(bucket, starts_at, ends_at)
JOIN s ON s.u = w.starts_at
LEFT JOIN ev ON ev.currency = s.currency AND ev.occurred_at >= w.starts_at AND ev.occurred_at < w.ends_at
  AND (ev.began IS NULL OR ev.began < w.starts_at)
GROUP BY ` + strings.Join(group, ", ")
	return stmt{family: FamMRRTrailing, sql: sql, args: b.args, leaves: leaves, hasTime: true, snapshot: true}, nil
}

// compileScheduled is what is still to happen: the scheduled movements dated
// after the as-of instant (a cancellation at period end, churn whatever the
// customer keeps) and the scheduled changes a renewal will bill, priced now.
func compileScheduled(plan *Plan, merchantID uuid.UUID, leaves []*Measure) (stmt, error) {
	b := &binder{}
	m := b.arg(merchantID)
	from, to := b.arg(plan.From), b.arg(plan.To)
	sel, group := []string{}, []string{}
	if plan.HasTime {
		sel, group = append(sel, "date_trunc("+b.arg(plan.Grain)+", x.at, "+b.arg(plan.zone().String())+") AS bucket"), append(group, "1")
	}
	spec := families[FamScheduled]
	for _, d := range plan.Dims {
		sel = append(sel, spec.DimExprs[d]+" AS dim_"+d)
		group = append(group, fmt.Sprint(len(sel)))
	}
	for _, l := range leaves {
		sel = append(sel, l.Expr+" AS m_"+l.Name)
	}
	var where []string
	for _, d := range sortedFilterDims(plan.Filters) {
		where = append(where, spec.DimExprs[d]+" = ANY("+b.arg(plan.Filters[d])+")")
	}
	sql := "SELECT " + strings.Join(sel, ",\n       ") + `
FROM (
  SELECT m.occurred_at AS at, m.currency, 'churn' AS kind, m.delta AS amount
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.occurred_at >= ` + from + ` AND m.occurred_at < ` + to + ` AND m.occurred_at > ` + b.arg(plan.AsOf) + ` AND m.scheduled
  UNION ALL
  SELECT c.effective_at, c.currency, CASE WHEN c.amount < 0 THEN 'contraction' ELSE 'expansion' END, c.amount
  FROM billing.mrr_scheduled_changes(` + m + `) c
  WHERE c.effective_at >= ` + from + ` AND c.effective_at < ` + to + `
) x`
	if len(where) > 0 {
		sql += "\nWHERE " + strings.Join(where, " AND ")
	}
	if len(group) > 0 {
		sql += "\nGROUP BY " + strings.Join(group, ", ")
	}
	return stmt{family: FamScheduled, sql: sql, args: b.args, leaves: leaves, hasTime: plan.HasTime}, nil
}

// compileCohort reads the movements by the cohort month (their cohort_at)
// and the months since it, in the merchant's zone: a cohort's value at the
// end of month k is its movements through that month. Its size is its
// members for basis subscribers, its MRR at the end of its own month for
// basis mrr. The cohorts are the months the range starts.
func compileCohort(plan *Plan, merchantID uuid.UUID, leaves []*Measure) (stmt, error) {
	b := &binder{}
	zone := b.arg(plan.zone().String())
	merchant, from, to := b.arg(merchantID), b.arg(plan.From), b.arg(plan.To)
	basis := `'subscribers'`
	bases := `(VALUES ('subscribers'))`
	if slices.Contains(plan.Dims, "basis") || plan.Filters["basis"] != nil {
		bases = `(VALUES ('subscribers'), ('mrr'))`
		basis = "b.basis"
	}
	var filters []string
	for _, d := range sortedFilterDims(plan.Filters) {
		col, ok := map[string]string{"currency": "g.currency", "cohort": "g.cohort_label", "months_since_start": "g.n::text", "basis": "b.basis"}[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: cohort lacks filter dimension %s (registry bug)", d)
		}
		filters = append(filters, fmt.Sprintf("%s = ANY(%s)", col, b.arg(plan.Filters[d])))
	}
	sel := []string{}
	for _, d := range plan.Dims {
		col, ok := map[string]string{"currency": "g.currency", "cohort": "g.cohort_label", "months_since_start": "g.n::text", "basis": basis}[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: cohort lacks dimension %s (registry bug)", d)
		}
		sel = append(sel, col+" AS dim_"+d)
	}
	value := func(through string) string {
		return `(SELECT COALESCE(SUM(CASE WHEN ` + basis + ` = 'mrr' THEN f.amount ELSE f.logos END), 0)::bigint FROM f
			WHERE f.currency = g.currency AND f.cohort = g.cohort AND f.month <= ` + through + `)`
	}
	for _, m := range leaves {
		switch m.Name {
		case "cohort_value":
			sel = append(sel, value("g.month")+" AS m_cohort_value")
		case "cohort_size":
			sel = append(sel, "CASE WHEN "+basis+" = 'mrr' THEN "+value("g.cohort")+
				" ELSE COALESCE((SELECT a.members FROM a WHERE a.currency = g.currency AND a.cohort = g.cohort), 0) END AS m_cohort_size")
		default:
			return stmt{}, fmt.Errorf("metrics: cohort has no leaf %s (registry bug)", m.Name)
		}
	}
	sql := `WITH f AS (
	SELECT m.currency, date_trunc('month', m.cohort_at, ` + zone + `) AS cohort, date_trunc('month', m.occurred_at, ` + zone + `) AS month,
	       SUM(m.logo_delta)::bigint AS logos, SUM(m.delta)::bigint AS amount
	FROM billing.mrr_movements m
	WHERE m.merchant_id = ` + merchant + ` AND m.cohort_at >= ` + from + ` AND m.cohort_at < ` + to + ` AND m.occurred_at < ` + b.arg(plan.asOf(plan.To)) + `
	GROUP BY 1, 2, 3
), a AS (
	SELECT m.currency, date_trunc('month', m.cohort_at, ` + zone + `) AS cohort, count(DISTINCT m.customer_id)::bigint AS members
	FROM billing.mrr_movements m
	WHERE m.merchant_id = ` + merchant + ` AND m.cohort_at >= ` + from + ` AND m.cohort_at < ` + to + `
	GROUP BY 1, 2
), g AS (
	SELECT k.currency, k.cohort, to_char(k.cohort AT TIME ZONE ` + zone + `, 'YYYY-MM') AS cohort_label, mm.n,
	       ((k.cohort AT TIME ZONE ` + zone + `) + make_interval(months => mm.n)) AT TIME ZONE ` + zone + ` AS month
	FROM (SELECT currency, cohort FROM f UNION SELECT currency, cohort FROM a) k
	CROSS JOIN LATERAL generate_series(0, 1200) mm(n)
	WHERE ((k.cohort AT TIME ZONE ` + zone + `) + make_interval(months => mm.n)) AT TIME ZONE ` + zone + ` < ` + to + `
)
SELECT ` + strings.Join(sel, ",\n       ") + `
FROM g CROSS JOIN ` + bases + ` b(basis)`
	if len(filters) > 0 {
		sql += "\nWHERE " + strings.Join(filters, " AND ")
	}
	return stmt{family: FamCohort, sql: sql, args: b.args, leaves: leaves}, nil
}

// compileRisk is at-risk MRR at each edge: before the as-of instant, the
// episodes open there (reason any counts a subscription once); from it on,
// what state says now. By risk_reason (grouped or filtered), each reason
// apart.
func compileRisk(plan *Plan, merchantID uuid.UUID, leaves []*Measure) (stmt, error) {
	if len(leaves) != 1 || leaves[0].Name != "mrr_at_risk" {
		return stmt{}, fmt.Errorf("metrics: risk compiles mrr_at_risk alone (registry bug)")
	}
	byReason := slices.Contains(plan.Dims, "risk_reason") || plan.Filters["risk_reason"] != nil
	var past, now []time.Time
	for _, e := range snapshotEdges(plan, leaves) {
		if e.Before(plan.AsOf) {
			past = append(past, e)
		} else {
			now = append(now, e)
		}
	}
	b := &binder{}
	m := b.arg(merchantID)
	var parts []string
	if len(past) > 0 {
		reason := "e.reason = 'any'"
		if byReason {
			reason = "e.reason <> 'any'"
		}
		last := b.arg(past[len(past)-1])
		src := snapshotSource{
			from: `(SELECT e.merchant_id, e.currency, e.reason, e.entered_at AS at, e.mrr_at_entry AS amount FROM billing.mrr_risk_episodes e
    WHERE e.merchant_id = ` + m + ` AND e.entered_at < ` + last + ` AND ` + reason + `
    UNION ALL
    SELECT e.merchant_id, e.currency, e.reason, e.left_at, -e.mrr_at_entry FROM billing.mrr_risk_episodes e
    WHERE e.merchant_id = ` + m + ` AND e.left_at < ` + last + ` AND ` + reason + `) x`,
			merchant: "x.merchant_id", at: "x.at",
			dims: map[string]string{"currency": "x.currency", "risk_reason": "x.reason"},
		}
		sql, err := snapshotSQL(plan, b, m, src, leaves, func(l *Measure) string { return l.Expr }, past)
		if err != nil {
			return stmt{}, err
		}
		parts = append(parts, sql)
	}
	if len(now) > 0 {
		at := b.arg(plan.AsOf)
		rows := `SELECT r.currency, r.mrr AS amount FROM billing.mrr_risk_now(` + m + `, ` + at + `) r WHERE r.merchant_id = ` + m
		if byReason {
			rows = `SELECT r.currency, x.reason, r.mrr AS amount FROM billing.mrr_risk_now(` + m + `, ` + at + `) r
    CROSS JOIN LATERAL unnest(r.reasons) AS x(reason) WHERE r.merchant_id = ` + m
		}
		sel, group, where := []string{"e.edge AS bucket"}, []string{"e.edge"}, []string{}
		for _, d := range plan.Dims {
			col := map[string]string{"currency": "x.currency", "risk_reason": "x.reason"}[d]
			sel, group = append(sel, col+" AS dim_"+d), append(group, col)
		}
		for _, d := range sortedFilterDims(plan.Filters) {
			col := map[string]string{"currency": "x.currency", "risk_reason": "x.reason"}[d]
			where = append(where, col+" = ANY("+b.arg(plan.Filters[d])+")")
		}
		sel = append(sel, "COALESCE(sum(x.amount), 0)::bigint AS m_mrr_at_risk")
		on := "true"
		if len(where) > 0 {
			on = strings.Join(where, " AND ")
		}
		join := "LEFT JOIN"
		if len(plan.Dims) > 0 {
			join = "JOIN"
		}
		parts = append(parts, "SELECT "+strings.Join(sel, ", ")+"\nFROM unnest("+b.arg(now)+"::timestamptz[]) AS e(edge)\n"+
			join+" ("+rows+") x ON "+on+"\nGROUP BY "+strings.Join(group, ", "))
	}
	sql := "(" + strings.Join(parts, ")\nUNION ALL\n(") + ")"
	return stmt{family: FamRisk, sql: sql, args: b.args, leaves: leaves, hasTime: true, snapshot: true}, nil
}
