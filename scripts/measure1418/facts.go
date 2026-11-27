package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// statement is one SQL statement of a read, with its arguments.
type statement struct {
	Name string
	SQL  string
	Args []any
}

type argList struct{ args []any }

func (a *argList) add(v any) string {
	a.args = append(a.args, v)
	return fmt.Sprintf("$%d", len(a.args))
}

// dimCol is a dimension's expression in a source.
type dimCol struct{ name, expr string }

// source is the rows a read sums: their table, their instant and the
// expressions of their dimensions and leaves.
type source struct {
	from  string
	where []string
	at    string
	dims  map[string]string
}

// Arms of the facts-only reads.
const (
	armFacts = "facts" // movements, lines, cases and episodes as they are
	armWide  = "facts+lines" // lines that carry their movement's instant and currency
	armState = "state" // at risk now from the subscriptions, cards and delinquency
)

// factsStatements is the facts-only read of one measure: what the metrics
// compiler would emit if it read the MRR facts. asof bounds what has
// happened: a movement dated after it (a scheduled churn) is not counted.
func factsStatements(p *metrics.Plan, measure, arm string, mid uuid.UUID, asof time.Time) ([]statement, error) {
	a := &argList{}
	m := a.add(mid)
	clip := func(t time.Time) time.Time {
		if t.After(asof) {
			return asof
		}
		return t
	}
	edges := p.Buckets
	if !p.HasTime {
		edges = []time.Time{p.To}
	}
	moves := source{from: "billing.mrr_movements m", where: []string{"m.merchant_id = " + m}, at: "m.occurred_at",
		dims: map[string]string{"currency": "m.currency", "movement": "m.kind"}}
	lines := source{from: "billing.mrr_movement_lines l JOIN billing.mrr_movements mv ON mv.merchant_id = l.merchant_id AND mv.id = l.movement_id",
		where: []string{"l.merchant_id = " + m, "mv.merchant_id = " + m}, at: "mv.occurred_at",
		dims: map[string]string{"currency": "mv.currency", "product_id": "'prod_' || l.product_id::text"}}
	lineDelta, lineSubs := "l.delta", "l.subscriber_delta"
	if arm == armWide {
		lines = source{from: "proto.lines_wide l", where: []string{"l.merchant_id = " + m}, at: "l.occurred_at",
			dims: map[string]string{"currency": "l.currency", "product_id": "'prod_' || l.product_id::text"}}
	}
	byProduct := slices.Contains(p.Dims, "product_id")
	one := func(sql string) []statement { return []statement{{Name: measure, SQL: sql, Args: a.args}} }

	switch measure {
	case "mrr", "arpa":
		leaves := []dimCol{{"mrr", "m.delta"}}
		if measure == "arpa" {
			leaves = append(leaves, dimCol{"subscribers", "m.logo_delta"})
		}
		if byProduct {
			return one(snapshotSQL(a, lines, p.Dims, nil, []dimCol{{"mrr", lineDelta}}, edges, clip(edges[len(edges)-1]))), nil
		}
		return one(snapshotSQL(a, moves, p.Dims, nil, leaves, edges, clip(edges[len(edges)-1]))), nil
	case "subscribers":
		if byProduct {
			return one(snapshotSQL(a, lines, p.Dims, nil, []dimCol{{"subscribers", lineSubs}}, edges, clip(edges[len(edges)-1]))), nil
		}
		if slices.Contains(p.Dims, "currency") {
			return one(snapshotSQL(a, moves, p.Dims, nil, []dimCol{{"subscribers", "m.logo_delta"}}, edges, clip(edges[len(edges)-1]))), nil
		}
		return one(distinctSubscribersSQL(a, m, edges, clip(edges[len(edges)-1]))), nil
	case "mrr_movements", "subscriber_movements":
		leaf := dimCol{"mrr_movements", "COALESCE(sum(m.delta), 0)::bigint"}
		if measure == "subscriber_movements" {
			leaf = dimCol{"subscriber_movements", "COALESCE(sum(m.logo_delta), 0)::bigint"}
		}
		return one(flowSQL(a, p, moves, []dimCol{leaf}, p.From, clip(p.To))), nil
	case "scheduled_mrr":
		return one(scheduledSQL(a, p, m, asof)), nil
	case "subscriber_churn_rate":
		snap := snapshotSQL(a, moves, nil, crossTypes(p), []dimCol{{"subscribers_at_period_start", "m.logo_delta"}}, edges, clip(edges[len(edges)-1]))
		first := statement{Name: "subscribers_at_period_start", SQL: snap, Args: a.args}
		b := &argList{}
		return []statement{first, {Name: "churn", SQL: churnSQL(b, p, mid, clip(p.To)), Args: b.args}}, nil
	case "nrr", "grr":
		snap := snapshotSQL(a, moves, p.Dims, nil, []dimCol{{"mrr_at_period_start", "m.delta"}}, edges, clip(edges[len(edges)-1]))
		first := statement{Name: "mrr_at_period_start", SQL: snap, Args: a.args}
		b := &argList{}
		return []statement{first, {Name: "base", SQL: baseSQL(b, p, mid, measure, clip(p.To)), Args: b.args}}, nil
	case "ltv":
		snap := snapshotSQL(a, moves, p.Dims, nil, []dimCol{{"mrr", "m.delta"}, {"subscribers", "m.logo_delta"}}, edges, clip(edges[len(edges)-1]))
		first := statement{Name: "arpa", SQL: snap, Args: a.args}
		b := &argList{}
		return []statement{first, {Name: "trailing", SQL: trailingSQL(b, p, mid, asof), Args: b.args}}, nil
	case "cohort_retention":
		return one(cohortSQL(a, p, m, clip(p.To))), nil
	case "mrr_at_risk":
		if arm == armState {
			return one(riskNowSQL(a, p, m, asof)), nil
		}
		last := a.add(clip(edges[len(edges)-1]))
		var src source
		if slices.Contains(p.Dims, "risk_reason") {
			src = source{from: `(SELECT e.merchant_id, e.currency, e.reason, e.entered_at AS at, e.mrr_at_entry AS amount FROM billing.mrr_risk_episodes e
				WHERE e.merchant_id = ` + m + ` AND e.entered_at < ` + last + `
				UNION ALL
				SELECT e.merchant_id, e.currency, e.reason, e.left_at, -e.mrr_at_entry FROM billing.mrr_risk_episodes e
				WHERE e.merchant_id = ` + m + ` AND e.left_at < ` + last + `) x`,
				where: []string{"x.merchant_id = " + m}, at: "x.at", dims: map[string]string{"currency": "x.currency", "risk_reason": "x.reason"}}
		} else {
			src = source{from: `(SELECT c.merchant_id, c.currency, c.opened_at AS at, c.mrr_at_entry AS amount FROM billing.mrr_risk_cases c
				WHERE c.merchant_id = ` + m + ` AND c.opened_at < ` + last + `
				UNION ALL
				SELECT c.merchant_id, c.currency, c.closed_at, -c.mrr_at_entry FROM billing.mrr_risk_cases c
				WHERE c.merchant_id = ` + m + ` AND c.closed_at < ` + last + `) x`,
				where: []string{"x.merchant_id = " + m}, at: "x.at", dims: map[string]string{"currency": "x.currency"}}
		}
		return one(snapshotSQL(a, src, p.Dims, nil, []dimCol{{"mrr_at_risk", "x.amount"}}, edges, clip(edges[len(edges)-1]))), nil
	case "at_risk_recovery_rate":
		src := source{from: "billing.mrr_risk_cases c", where: []string{"c.merchant_id = " + m}, at: "c.closed_at",
			dims: map[string]string{"currency": "c.currency", "risk_reason": "c.entry_reason"}}
		return one(flowSQL(a, p, src, []dimCol{
			{"at_risk_recovered_mrr", "COALESCE(sum(c.mrr_at_exit) FILTER (WHERE c.outcome = 'recovered'), 0)::bigint"},
			{"at_risk_closed_mrr", "COALESCE(sum(c.mrr_at_exit) FILTER (WHERE c.outcome IN ('recovered', 'churned')), 0)::bigint"},
		}, p.From, clip(p.To))), nil
	}
	return nil, fmt.Errorf("no facts read for %s", measure)
}

var churnTypes = []string{"voluntary", "involuntary", "chargeback"}

func crossTypes(p *metrics.Plan) []string {
	if slices.Contains(p.Dims, "churn_type") {
		return []string{"churn_type"}
	}
	return nil
}

// snapshotSQL is each leaf's sum over the source's rows dated before each
// edge: one pass over the rows before the last edge, numbered by the edges
// they precede (width_bucket), then summed per edge. cross dims (churn_type)
// repeat every row.
func snapshotSQL(a *argList, src source, dims, cross []string, leaves []dimCol, edges []time.Time, last time.Time) string {
	e, l := a.add(edges), a.add(last)
	var own []string
	for _, d := range dims {
		if !slices.Contains(cross, d) {
			own = append(own, d)
		}
	}
	var gsel, gby, dsel, join, osel, oby []string
	for i, d := range own {
		gsel = append(gsel, src.dims[d]+" AS d"+fmt.Sprint(i))
		gby = append(gby, fmt.Sprint(i+1))
		dsel = append(dsel, "d"+fmt.Sprint(i))
		join = append(join, fmt.Sprintf("g.d%d = d.d%d", i, i))
		osel = append(osel, fmt.Sprintf("d.d%d AS dim_%s", i, d))
		oby = append(oby, fmt.Sprintf("d.d%d", i))
	}
	for _, d := range cross {
		osel = append(osel, "ct.v AS dim_"+d)
		oby = append(oby, "ct.v")
	}
	gby = append(gby, fmt.Sprint(len(own)+1))
	gsel = append(gsel, "width_bucket("+src.at+", "+e+"::timestamptz[]) AS k")
	for i, lf := range leaves {
		gsel = append(gsel, fmt.Sprintf("sum(%s) AS l%d", lf.expr, i))
		osel = append(osel, fmt.Sprintf("COALESCE(sum(g.l%d), 0)::bigint AS m_%s", i, lf.name))
	}
	dpart := "(SELECT 1 AS one) d"
	if len(dsel) > 0 {
		dpart = "(SELECT DISTINCT " + strings.Join(dsel, ", ") + " FROM g) d"
	}
	join = append(join, "g.k < e.k")
	crossJoin := ""
	if len(cross) > 0 {
		crossJoin = "\nCROSS JOIN (VALUES ('voluntary'), ('involuntary'), ('chargeback')) ct(v)"
	}
	where := append(slices.Clone(src.where), src.at+" < "+l)
	return "WITH g AS (\n  SELECT " + strings.Join(gsel, ", ") +
		"\n  FROM " + src.from + "\n  WHERE " + strings.Join(where, " AND ") +
		"\n  GROUP BY " + strings.Join(gby, ", ") + "\n)" +
		"\nSELECT " + strings.Join(append([]string{"e.edge AS bucket"}, osel...), ", ") +
		"\nFROM unnest(" + e + "::timestamptz[]) WITH ORDINALITY AS e(edge, k)\nCROSS JOIN " + dpart + crossJoin +
		"\nLEFT JOIN g ON " + strings.Join(join, " AND ") +
		"\nGROUP BY " + strings.Join(append([]string{"e.edge"}, oby...), ", ")
}

// distinctSubscribersSQL counts each customer once across currencies: a
// customer moves the count when its currencies with MRR go from none to one
// or from one to none, in the order of its movements.
func distinctSubscribersSQL(a *argList, m string, edges []time.Time, last time.Time) string {
	e, l := a.add(edges), a.add(last)
	return `WITH x AS (
  SELECT m.occurred_at, m.logo_delta,
         sum(m.logo_delta) OVER (PARTITION BY m.customer_id ORDER BY m.occurred_at, m.id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS n
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.logo_delta <> 0 AND m.occurred_at < ` + l + `
), g AS (
  SELECT width_bucket(x.occurred_at, ` + e + `::timestamptz[]) AS k, sum((x.n > 0)::integer - (x.n - x.logo_delta > 0)::integer) AS d
  FROM x GROUP BY 1
)
SELECT e.edge AS bucket, COALESCE(sum(g.d), 0)::bigint AS m_subscribers
FROM unnest(` + e + `::timestamptz[]) WITH ORDINALITY AS e(edge, k)
LEFT JOIN g ON g.k < e.k
GROUP BY e.edge`
}

// flowSQL sums the source's rows dated in [from, to) by bucket and dims.
func flowSQL(a *argList, p *metrics.Plan, src source, leaves []dimCol, from, to time.Time) string {
	sel, by := []string{}, []string{}
	if p.HasTime {
		sel = append(sel, "date_trunc("+a.add(p.Grain)+", "+src.at+", "+a.add(zoneName(p))+") AS bucket")
		by = append(by, "1")
	}
	for _, d := range p.Dims {
		sel = append(sel, src.dims[d]+" AS dim_"+d)
		by = append(by, fmt.Sprint(len(sel)))
	}
	for _, lf := range leaves {
		sel = append(sel, lf.expr+" AS m_"+lf.name)
	}
	where := append(slices.Clone(src.where), src.at+" >= "+a.add(from), src.at+" < "+a.add(to))
	sql := "SELECT " + strings.Join(sel, ", ") + "\nFROM " + src.from + "\nWHERE " + strings.Join(where, " AND ")
	if len(by) > 0 {
		sql += "\nGROUP BY " + strings.Join(by, ", ")
	}
	return sql
}

// scheduledSQL is what is still to happen: movements dated after asof (a
// scheduled churn, counted once its date passes) and the scheduled price
// changes a renewal will bill.
func scheduledSQL(a *argList, p *metrics.Plan, m string, asof time.Time) string {
	from, to, now := a.add(p.From), a.add(p.To), a.add(asof)
	sel, by := []string{"date_trunc(" + a.add(p.Grain) + ", x.at, " + a.add(zoneName(p)) + ") AS bucket"}, []string{"1"}
	for _, d := range p.Dims {
		col := map[string]string{"currency": "x.currency", "movement": "x.kind"}[d]
		sel = append(sel, col+" AS dim_"+d)
		by = append(by, fmt.Sprint(len(sel)))
	}
	sel = append(sel, "COALESCE(sum(x.delta), 0)::bigint AS m_scheduled_mrr")
	return "SELECT " + strings.Join(sel, ", ") + `
FROM (
  SELECT m.occurred_at AS at, m.currency, 'churn'::text AS kind, m.delta FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.occurred_at > ` + now + ` AND m.occurred_at >= ` + from + ` AND m.occurred_at < ` + to + `
  UNION ALL
  SELECT ms.effective_at, ms.currency, ms.kind, ms.mrr_delta FROM billing.mrr_scheduled ms
  WHERE ms.merchant_id = ` + m + ` AND ms.status = 'scheduled' AND ms.kind <> 'churn' AND ms.effective_at >= ` + from + ` AND ms.effective_at < ` + to + `
) x
GROUP BY ` + strings.Join(by, ", ")
}

// fresh is true for a movement whose customer gained MRR from none in the
// same bucket at or before it: its run began in the period, so it is not
// the opening base's.
const fresh = `bool_or(m.logo_delta = 1) OVER (PARTITION BY m.customer_id, m.currency, date_trunc(%[1]s, m.occurred_at, %[2]s)
         ORDER BY m.occurred_at, m.id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)`

// churnSQL counts, per bucket, the opening base's churn (by type) and its
// customers a prune took out.
func churnSQL(a *argList, p *metrics.Plan, mid uuid.UUID, to time.Time) string {
	m, g, z := a.add(mid), a.add(p.Grain), a.add(zoneName(p))
	start, from, end := a.add(p.Buckets[0]), a.add(p.From), a.add(to)
	typ := `CASE WHEN mv.voluntary THEN 'voluntary' WHEN mv.cause = 'chargeback' THEN 'chargeback' ELSE 'involuntary' END`
	sel, by, cross, match := []string{"mv.b AS bucket"}, []string{"1"}, "", ""
	if slices.Contains(p.Dims, "churn_type") {
		sel, by = append(sel, "ct.v AS dim_churn_type"), append(by, "2")
		cross, match = "\nCROSS JOIN (VALUES ('voluntary'), ('involuntary'), ('chargeback')) ct(v)", " AND "+typ+" = ct.v"
	}
	sel = append(sel,
		"COALESCE(sum(-mv.logo_delta) FILTER (WHERE mv.inr AND NOT mv.fresh AND mv.kind = 'churn'"+match+"), 0)::bigint AS m_churned_subscribers",
		"COALESCE(sum(-mv.logo_delta) FILTER (WHERE mv.inr AND NOT mv.fresh AND mv.kind = 'correction'), 0)::bigint AS m_base_pruned")
	return `WITH mv AS (
  SELECT m.kind, m.logo_delta, m.voluntary, m.cause, m.occurred_at >= ` + from + ` AS inr, date_trunc(` + g + `, m.occurred_at, ` + z + `) AS b,
         ` + fmt.Sprintf(fresh, g, z) + ` AS fresh
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.logo_delta <> 0 AND m.occurred_at >= ` + start + ` AND m.occurred_at < ` + end + `
)
SELECT ` + strings.Join(sel, ", ") + `
FROM mv` + cross + `
GROUP BY ` + strings.Join(by, ", ")
}

// baseSQL sums, per bucket and currency, the opening base's expansion,
// contraction, churn and corrections: nrr's and grr's movements.
func baseSQL(a *argList, p *metrics.Plan, mid uuid.UUID, measure string, to time.Time) string {
	m, g, z := a.add(mid), a.add(p.Grain), a.add(zoneName(p))
	start, from, end := a.add(p.Buckets[0]), a.add(p.From), a.add(to)
	kinds := "'expansion', 'contraction', 'churn'"
	leaf := "base_mrr_drop"
	if measure == "grr" {
		kinds, leaf = "'contraction', 'churn'", "base_mrr_lost"
	}
	return `WITH mv AS (
  SELECT m.currency, m.kind, m.delta, m.occurred_at >= ` + from + ` AS inr, date_trunc(` + g + `, m.occurred_at, ` + z + `) AS b,
         ` + fmt.Sprintf(fresh, g, z) + ` AS fresh
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.occurred_at >= ` + start + ` AND m.occurred_at < ` + end + `
)
SELECT mv.b AS bucket, mv.currency AS dim_currency,
  COALESCE(-sum(mv.delta) FILTER (WHERE mv.inr AND NOT mv.fresh AND mv.kind IN (` + kinds + `)), 0)::bigint AS m_` + leaf + `,
  COALESCE(-sum(mv.delta) FILTER (WHERE mv.inr AND NOT mv.fresh AND mv.kind = 'correction'), 0)::bigint AS m_base_mrr_corrected
FROM mv
GROUP BY 1, 2`
}

// trailingSQL is LTV's churn: per bucket, the three months ending at its end;
// the base is the customers with MRR at the window's start less those a
// prune took out, the churn the base's. A movement's run began at the
// customer's latest gain from none at or before it within the rows read
// (none read: before them all).
func trailingSQL(a *argList, p *metrics.Plan, mid uuid.UUID, asof time.Time) string {
	labels := []time.Time{p.To}
	if p.HasTime {
		labels = p.Buckets
	}
	starts, ends := make([]time.Time, len(labels)), make([]time.Time, len(labels))
	for i, b := range labels {
		starts[i], ends[i] = trailingWindow(p, b)
	}
	uniq := slices.Clone(starts)
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Before(uniq[j]) })
	uniq = slices.CompactFunc(uniq, func(x, y time.Time) bool { return x.Equal(y) })
	lo, hi := uniq[0], ends[0]
	for _, e := range ends {
		if e.After(hi) {
			hi = e
		}
	}
	if hi.After(asof) {
		hi = asof
	}
	m := a.add(mid)
	w := "unnest(" + a.add(labels) + "::timestamptz[], " + a.add(starts) + "::timestamptz[], " + a.add(ends) + "::timestamptz[]) AS w(bucket, starts_at, ends_at)"
	u, ulast := a.add(uniq), a.add(uniq[len(uniq)-1])
	return `WITH mv AS (
  SELECT m.currency, m.kind, m.logo_delta, m.occurred_at,
         max(m.occurred_at) FILTER (WHERE m.logo_delta = 1) OVER (PARTITION BY m.customer_id, m.currency ORDER BY m.occurred_at, m.id
           ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS began
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.logo_delta <> 0 AND m.occurred_at >= ` + a.add(lo) + ` AND m.occurred_at < ` + a.add(hi) + `
), ev AS (
  SELECT * FROM mv WHERE mv.kind IN ('churn', 'correction')
), g AS (
  SELECT m.currency, width_bucket(m.occurred_at, ` + u + `::timestamptz[]) AS k, sum(m.logo_delta) AS d
  FROM billing.mrr_movements m
  WHERE m.merchant_id = ` + m + ` AND m.logo_delta <> 0 AND m.occurred_at < ` + ulast + `
  GROUP BY 1, 2
), s AS (
  SELECT c.currency, uu.u, COALESCE(sum(g.d), 0) AS subs
  FROM unnest(` + u + `::timestamptz[]) WITH ORDINALITY AS uu(u, k)
  CROSS JOIN (SELECT DISTINCT currency FROM g) c
  LEFT JOIN g ON g.currency = c.currency AND g.k < uu.k
  GROUP BY 1, 2
)
SELECT w.bucket, s.currency AS dim_currency,
  (s.subs + COALESCE(sum(ev.logo_delta) FILTER (WHERE ev.kind = 'correction'), 0))::bigint AS m_trailing_base,
  COALESCE(count(*) FILTER (WHERE ev.kind = 'churn'), 0)::bigint AS m_trailing_churned
FROM ` + w + `
JOIN s ON s.u = w.starts_at
LEFT JOIN ev ON ev.currency = s.currency AND ev.occurred_at >= w.starts_at AND ev.occurred_at < w.ends_at
  AND (ev.began IS NULL OR ev.began < w.starts_at)
GROUP BY w.bucket, s.currency, s.subs`
}

// trailingWindow is the metrics compiler's: the three months ending at the
// end of the bucket labelled b, on the zone's calendar.
func trailingWindow(p *metrics.Plan, b time.Time) (time.Time, time.Time) {
	end := p.To
	if p.HasTime {
		if i := slices.IndexFunc(p.Buckets, func(x time.Time) bool { return x.Equal(b) }); i >= 0 && i+1 < len(p.Buckets) {
			end = p.Buckets[i+1]
		}
	}
	zone := p.Zone
	if zone == nil {
		zone = time.UTC
	}
	local := end.In(zone)
	y, mo, d := local.Date()
	dayStart := calendarutil.DayStart(y, mo, d, zone)
	sy, sm, sd := time.Date(y, mo-3, d, 0, 0, 0, 0, time.UTC).Date()
	return calendarutil.DayStart(sy, sm, sd, zone).Add(end.Sub(dayStart)), end
}

// cohortSQL is the compiler's cohort read with its two sources swapped for
// the movements: each cohort's movements by month, and its members.
func cohortSQL(a *argList, p *metrics.Plan, m string, to time.Time) string {
	zone := a.add(zoneName(p))
	from, end, cut := a.add(p.From), a.add(p.To), a.add(to)
	sel := []string{}
	for _, d := range p.Dims {
		sel = append(sel, map[string]string{"currency": "g.currency", "cohort": "g.cohort_label", "months_since_start": "g.n::text", "basis": "'subscribers'"}[d]+" AS dim_"+d)
	}
	value := func(through string) string {
		return `(SELECT COALESCE(SUM(f.logos), 0)::bigint FROM f WHERE f.currency = g.currency AND f.cohort = g.cohort AND f.month <= ` + through + `)`
	}
	sel = append(sel, value("g.month")+" AS m_cohort_value",
		"COALESCE((SELECT a.members FROM a WHERE a.currency = g.currency AND a.cohort = g.cohort), 0) AS m_cohort_size")
	return `WITH f AS (
	SELECT m.currency, date_trunc('month', m.cohort_at, ` + zone + `) AS cohort, date_trunc('month', m.occurred_at, ` + zone + `) AS month,
	       SUM(m.logo_delta)::bigint AS logos, SUM(m.delta)::bigint AS amount
	FROM billing.mrr_movements m
	WHERE m.merchant_id = ` + m + ` AND m.cohort_at >= ` + from + ` AND m.cohort_at < ` + end + ` AND m.occurred_at < ` + cut + `
	GROUP BY 1, 2, 3
), a AS (
	SELECT m.currency, date_trunc('month', m.cohort_at, ` + zone + `) AS cohort, count(DISTINCT m.customer_id)::bigint AS members
	FROM billing.mrr_movements m
	WHERE m.merchant_id = ` + m + ` AND m.cohort_at >= ` + from + ` AND m.cohort_at < ` + end + `
	GROUP BY 1, 2
), g AS (
	SELECT k.currency, k.cohort, to_char(k.cohort AT TIME ZONE ` + zone + `, 'YYYY-MM') AS cohort_label, mm.n,
	       ((k.cohort AT TIME ZONE ` + zone + `) + make_interval(months => mm.n)) AT TIME ZONE ` + zone + ` AS month
	FROM (SELECT currency, cohort FROM f UNION SELECT currency, cohort FROM a) k
	CROSS JOIN LATERAL generate_series(0, 1200) mm(n)
	WHERE ((k.cohort AT TIME ZONE ` + zone + `) + make_interval(months => mm.n)) AT TIME ZONE ` + zone + ` < ` + end + `
)
SELECT ` + strings.Join(sel, ",\n       ") + `
FROM g`
}

// riskNowSQL is at-risk MRR now computed from current state alone: the
// subscriptions adding MRR whose status, card or customer's delinquency puts
// them at risk, without subscription_mrr.risk or any case.
func riskNowSQL(a *argList, p *metrics.Plan, m string, asof time.Time) string {
	now := a.add(asof)
	sel, by := []string{a.add(p.To) + "::timestamptz AS bucket"}, []string{}
	for _, d := range p.Dims {
		sel = append(sel, map[string]string{"currency": "pr.currency"}[d]+" AS dim_"+d)
		by = append(by, fmt.Sprint(len(sel)))
	}
	sel = append(sel, "COALESCE(sum(sm.mrr), 0)::bigint AS m_mrr_at_risk")
	sql := "SELECT " + strings.Join(sel, ", ") + `
FROM billing.subscription_mrr sm
JOIN billing.subscriptions s ON s.merchant_id = sm.merchant_id AND s.id = sm.subscription_id
JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id
LEFT JOIN billing.payment_methods pm ON pm.merchant_id = s.merchant_id
  AND pm.id = CASE s.collection_policy
      WHEN 'engine' THEN billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy)
      WHEN 'nmi_schedule' THEN s.payment_method_id END
WHERE sm.merchant_id = ` + m + ` AND sm.mrr > 0 AND (sm.ends_at IS NULL OR sm.ends_at > ` + now + `) AND s.deleted_at IS NULL
  AND (s.status IN ('past_due', 'awaiting_method', 'unverified')
    OR (make_date(pm.card_exp_year, pm.card_exp_month, 1) + interval '1 month') AT TIME ZONE 'UTC' <= s.current_period_ends_at
    OR EXISTS (SELECT 1 FROM billing.customer_delinquency d WHERE d.merchant_id = s.merchant_id AND d.customer_id = s.customer_id
               AND d.currency = pr.currency AND d.state = 'delinquent'))`
	if len(by) > 0 {
		sql += "\nGROUP BY " + strings.Join(by, ", ")
	}
	return sql
}

func zoneName(p *metrics.Plan) string {
	if p.Zone == nil {
		return "UTC"
	}
	return p.Zone.String()
}
