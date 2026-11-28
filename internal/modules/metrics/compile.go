package metrics

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// stmt is one compiled family statement.
type stmt struct {
	family   Family
	sql      string
	args     []any
	leaves   []*Measure // measure columns, in select order
	hasTime  bool
	snapshot bool // snapshot/depletion: bucket column is the edge; balance: cumulative
	balance  bool
}

// Statement is one SQL statement Execute runs for a plan, in the billing
// schema, with its arguments.
type Statement struct {
	Family Family
	SQL    string
	Args   []any
}

// Statements are the plan's statements for merchantID, the depletion
// family's prior read aside: what a plan reads, for EXPLAIN.
func Statements(plan *Plan, merchantID uuid.UUID) ([]Statement, error) {
	stmts, err := compile(plan, merchantID, nil)
	if err != nil {
		return nil, err
	}
	out := make([]Statement, len(stmts))
	for i, st := range stmts {
		out[i] = Statement{Family: st.family, SQL: st.sql, Args: st.args}
	}
	return out, nil
}

// compile builds one parameterized statement per source family involved.
// Every SQL fragment comes from the registry; client values ride in args.
// Usage families are the event store's (usage.go) and compile to none;
// burns are the usage the depletion measure reads.
func compile(plan *Plan, merchantID uuid.UUID, burns []burn) ([]stmt, error) {
	// Group leaf components by family, preserving first-seen order. Snapshot
	// leaves with different edge semantics (period-start components like churn's
	// denominator) get their own statement in the non-time case.
	type famKey struct {
		fam         Family
		periodStart bool
	}
	famOrder := []famKey{}
	famLeaves := map[famKey][]*Measure{}
	seen := map[string]bool{}
	for _, m := range plan.Measures {
		for _, leaf := range m.components() {
			if seen[leaf.Name] {
				continue
			}
			seen[leaf.Name] = true
			k := famKey{fam: leaf.Family, periodStart: !plan.HasTime && leaf.SnapshotAtPeriodStart}
			if _, ok := famLeaves[k]; !ok {
				famOrder = append(famOrder, k)
			}
			famLeaves[k] = append(famLeaves[k], leaf)
		}
	}

	var stmts []stmt
	for _, k := range famOrder {
		fam := k.fam
		leaves := famLeaves[k]
		if storeFamily(fam) || fam == FamRunoff {
			continue
		}
		spec := families[fam]
		var st stmt
		var err error
		switch spec.Kind {
		case "flow":
			st, err = compileFlow(plan, merchantID, fam, spec, leaves)
		case "snapshot":
			st, err = compileSnapshot(plan, merchantID, fam, spec, leaves)
		case "risk":
			st, err = compileRisk(plan, merchantID, leaves)
		case "mrr":
			mrr, merr := compileMRR(plan, merchantID, leaves)
			if merr != nil {
				return nil, merr
			}
			stmts = append(stmts, mrr...)
			continue
		case "mrr_base":
			st, err = compileMRRBase(plan, merchantID, leaves)
		case "scheduled":
			st, err = compileScheduled(plan, merchantID, leaves)
		case "cohort":
			st, err = compileCohort(plan, merchantID, leaves)
		case "trailing":
			st, err = compileTrailing(plan, merchantID, leaves)
		case "ledger_balance":
			st, err = compileLedgerBalance(plan, merchantID, fam, spec, leaves)
		case "depletion":
			st, err = compileDepletion(plan, merchantID, leaves, burns)
		default:
			err = fmt.Errorf("metrics: family %s has no skeleton", fam)
		}
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, st)
	}
	return stmts, nil
}

// merchantCols: explicit merchant predicate per family (the merchant scope,
// and it anchors the (merchant_id, time) indexes).
var merchantCols = map[Family]string{
	FamPayments:       "p.merchant_id",
	FamGrants:         "g.merchant_id",
	FamLimitsReached:  "lw.merchant_id",
	FamSubsSnapshot:   "s.merchant_id",
	FamEntitlSnapshot: "e.merchant_id",
	FamRevenue:        "ld.merchant_id",
	FamCreditsSold:    "ld.merchant_id",
	FamBalances:       "x.merchant_id",
	FamWebhookHealth:  "wh.merchant_id",
	FamWebhookDaily:   "whd.merchant_id",
	FamAttempts:       "a.merchant_id",
	FamPurchases:      "ck.merchant_id",
	FamRebillCycles:   "cy.merchant_id",
	FamFindings:       "f.merchant_id",
	FamCheckouts:      "o.merchant_id",
	FamSales:          "o.merchant_id",
	FamDisputes:       "d.merchant_id",
	FamFraudReports:   "fr.merchant_id",
	FamNetwork:        "na.merchant_id",
	FamNetworkPrior:   "nap.merchant_id",
	FamRisk:           "e.merchant_id",
	FamRiskFlows:      "c.merchant_id",
	FamMRR:            "m.merchant_id",
	FamMRRTrailing:    "m.merchant_id",
	FamMRRFlows:       "m.merchant_id",
	FamMRRBase:        "m.merchant_id",
	FamScheduled:      "m.merchant_id",
	FamCohort:         "m.merchant_id",
	FamCancellations:  "sc.merchant_id",
}

func compileFlow(plan *Plan, merchantID uuid.UUID, fam Family, spec familySpec, leaves []*Measure) (stmt, error) {
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	sel := []string{}
	group := []string{}
	if plan.HasTime {
		sel = append(sel, bucketExpr(spec, arg(plan.Grain), arg(plan.zone().String()))+" AS bucket")
		group = append(group, "1")
	}
	joins := map[string]bool{}
	for _, d := range plan.Dims {
		expr, ok := spec.DimExprs[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: family %s lacks dimension %s (registry bug)", fam, d)
		}
		if j, ok := spec.DimJoins[d]; ok {
			joins[j] = true
		}
		sel = append(sel, expr+" AS dim_"+d)
		group = append(group, fmt.Sprintf("%d", len(sel)))
	}
	for _, m := range leaves {
		sel = append(sel, m.Expr+" AS m_"+m.Name)
	}

	where := []string{merchantCols[fam] + " = " + arg(merchantID)}
	if spec.BaseWhere != "" {
		where = append(where, spec.BaseWhere)
	}
	from, to := plan.From, plan.To
	if spec.Calendar {
		from, to = calendarBound(from, plan.zone(), false), calendarBound(to, plan.zone(), true)
	}

	where = append(where,
		fmt.Sprintf("%s >= %s", spec.TimeExpr, arg(from)),
		fmt.Sprintf("%s < %s", spec.TimeExpr, arg(to)))
	if spec.AsOf {
		where = append(where, fmt.Sprintf("%s <= %s", spec.TimeExpr, arg(plan.AsOf)))
	}
	for _, d := range sortedFilterDims(plan.Filters) {
		expr, ok := spec.DimExprs[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: family %s lacks filter dimension %s (registry bug)", fam, d)
		}
		if j, ok := spec.DimJoins[d]; ok {
			joins[j] = true
		}
		where = append(where, fmt.Sprintf("%s = ANY(%s)", expr, arg(plan.Filters[d])))
	}

	sql := "SELECT " + strings.Join(sel, ",\n       ") +
		"\nFROM " + spec.From + joinClauses(spec, joins) +
		"\nWHERE " + strings.Join(where, "\n  AND ")
	if len(group) > 0 {
		sql += "\nGROUP BY " + strings.Join(group, ", ")
	}
	// @to is the range end, for states settled as of the query.
	if strings.Contains(sql, "@to") {
		sql = strings.ReplaceAll(sql, "@to", arg(plan.To)+"::timestamptz")
	}
	// @period_start is the start of the row's bucket, or of the range.
	if strings.Contains(sql, "@period_start") {
		var start string
		if plan.HasTime {
			start = bucketExpr(spec, arg(plan.Grain), arg(plan.zone().String()))
		} else {
			start = arg(plan.From) + "::timestamptz"
		}
		sql = strings.ReplaceAll(sql, "@period_start", start)
	}
	return stmt{family: fam, sql: sql, args: args, leaves: leaves, hasTime: plan.HasTime}, nil
}

func compileSnapshot(plan *Plan, merchantID uuid.UUID, fam Family, spec familySpec, leaves []*Measure) (stmt, error) {
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	edges := snapshotEdges(plan, leaves)

	sel := []string{"edge.bucket AS bucket"}
	group := []string{"1"}
	joins := map[string]bool{}
	on := []string{merchantCols[fam] + " = " + arg(merchantID)}
	if spec.BaseWhere != "" {
		on = append(on, spec.BaseWhere)
	}
	for _, d := range plan.Dims {
		expr, ok := spec.DimExprs[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: family %s lacks dimension %s (registry bug)", fam, d)
		}
		if j, ok := spec.DimJoins[d]; ok {
			joins[j] = true
		}
		sel = append(sel, expr+" AS dim_"+d)
		group = append(group, fmt.Sprintf("%d", len(sel)))
	}
	for _, m := range leaves {
		sel = append(sel, m.Expr+" AS m_"+m.Name)
	}
	for _, d := range sortedFilterDims(plan.Filters) {
		expr, ok := spec.DimExprs[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: family %s lacks filter dimension %s (registry bug)", fam, d)
		}
		if j, ok := spec.DimJoins[d]; ok {
			joins[j] = true
		}
		on = append(on, fmt.Sprintf("%s = ANY(%s)", expr, arg(plan.Filters[d])))
	}

	// A parenthesized join tree is only valid SQL when it actually contains a
	// JOIN; a bare table ref joins unwrapped.
	joined := spec.From + joinClauses(spec, joins)
	if strings.Contains(joined, "JOIN") {
		joined = "(" + joined + ")"
	}
	sql := "SELECT " + strings.Join(sel, ",\n       ") +
		"\nFROM unnest(" + arg(edges) + "::timestamptz[]) AS edge(bucket)" +
		"\nLEFT JOIN " + joined + " ON " + strings.Join(on, "\n  AND ") +
		"\nGROUP BY " + strings.Join(group, ", ")
	return stmt{family: fam, sql: sql, args: args, leaves: leaves, hasTime: true, snapshot: true}, nil
}

// bucketExpr is the start of a row's bucket at grain, on the calendar of
// zone, as an instant: what calendarutil.Buckets gives. A calendar family's day is
// its date wherever the zone is.
func bucketExpr(spec familySpec, grain, zone string) string {
	if spec.Calendar {
		return fmt.Sprintf("(date_trunc(%s, (%s) AT TIME ZONE 'UTC') AT TIME ZONE %s)", grain, spec.TimeExpr, zone)
	}
	return fmt.Sprintf("date_trunc(%s, %s, %s)", grain, spec.TimeExpr, zone)
}

// calendarBound is the UTC midnight of t's date in zone, the date after when
// up and t is not that day's start: a calendar family's rows are keyed so.
func calendarBound(t time.Time, zone *time.Location, up bool) time.Time {
	y, m, d := t.In(zone).Date()
	if up && !t.Equal(calendarutil.DayStart(y, m, d, zone)) {
		d++
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// byProduct reports a query that groups or filters MRR by product: the
// product flows answer it.
func byProduct(plan *Plan) bool {
	return slices.Contains(plan.Dims, "product_id") || plan.Filters["product_id"] != nil
}

func byCurrency(plan *Plan) bool {
	return slices.Contains(plan.Dims, "currency") || plan.Filters["currency"] != nil
}

// trailingWindow is the three months ending at the end of the bucket
// labelled b (the range's end for the range's own label), on the zone's
// calendar.
func trailingWindow(plan *Plan, b time.Time) (time.Time, time.Time) {
	end := plan.To
	if plan.HasTime {
		if i := slices.IndexFunc(plan.Buckets, func(x time.Time) bool { return x.Equal(b) }); i >= 0 && i+1 < len(plan.Buckets) {
			end = plan.Buckets[i+1]
		}
	}
	local := end.In(plan.zone())
	y, m, d := local.Date()
	day := calendarutil.DayStart(y, m, d, plan.zone())
	sy, sm, sd := time.Date(y, m-3, d, 0, 0, 0, 0, time.UTC).Date()
	return calendarutil.DayStart(sy, sm, sd, plan.zone()).Add(end.Sub(day)), end
}

// compileDepletion: customers whose prepaid balance at the edge covers <= N days
// of their trailing-7d burn, in any currency. Bounded: edges x customers. The
// burns are read from the event store before.
func compileDepletion(plan *Plan, merchantID uuid.UUID, leaves []*Measure, burns []burn) (stmt, error) {
	edges := snapshotEdges(plan, leaves)
	burnEdges, burnCustomers, burnCurrencies, burnAmounts := make([]time.Time, 0, len(burns)), make([]uuid.UUID, 0, len(burns)), make([]string, 0, len(burns)), make([]int64, 0, len(burns))
	for _, b := range burns {
		burnEdges, burnCustomers, burnCurrencies, burnAmounts = append(burnEdges, b.edge), append(burnCustomers, b.customer), append(burnCurrencies, b.currency), append(burnAmounts, b.amount)
	}
	sql := fmt.Sprintf(`SELECT edge.bucket AS bucket, COUNT(DISTINCT x.customer_id) AS m_customers_at_depletion_risk
FROM unnest($2::timestamptz[]) AS edge(bucket)
LEFT JOIN LATERAL (
  SELECT bal.customer_id
  FROM (
    SELECT la.customer_id, la.currency,
           SUM(CASE WHEN lt.credit_account_id = la.id THEN lt.amount ELSE -lt.amount END) AS balance
    FROM billing.ledger_accounts la
    JOIN billing.ledger_transfers lt
      ON (lt.credit_account_id = la.id OR lt.debit_account_id = la.id)
     AND lt.merchant_id = la.merchant_id
     AND lt.created_at < edge.bucket
    WHERE la.merchant_id = $1 AND la.account_type = 'customer_units' AND la.customer_id IS NOT NULL
    GROUP BY la.customer_id, la.currency
  ) bal
  JOIN unnest($3::timestamptz[], $4::uuid[], $5::text[], $6::bigint[]) AS burn(edge, customer_id, currency, burn)
    ON burn.edge = edge.bucket AND burn.customer_id = bal.customer_id AND burn.currency = bal.currency
  WHERE burn.burn > 0 AND bal.balance <= (burn.burn / 7.0) * %d
) x ON TRUE
GROUP BY edge.bucket`, depletionRiskDays)
	return stmt{
		family: FamDepletion, sql: sql, args: []any{merchantID, edges, burnEdges, burnCustomers, burnCurrencies, burnAmounts},
		leaves: leaves, hasTime: true, snapshot: true,
	}, nil
}

// snapshotEdges: with a time grouping, snapshots evaluate at every bucket START
// label; without one, at the range end (or range start for period-start
// components like churn's denominator).
func snapshotEdges(plan *Plan, leaves []*Measure) []time.Time {
	if plan.HasTime {
		return plan.Buckets
	}
	atStart := false
	for _, m := range leaves {
		if m.SnapshotAtPeriodStart {
			atStart = true
		}
	}
	if atStart {
		return []time.Time{plan.From}
	}
	return []time.Time{plan.To}
}

func joinClauses(spec familySpec, joins map[string]bool) string {
	if len(joins) == 0 {
		return ""
	}
	// Deterministic order: registry-declared join snippets sorted.
	var out []string
	for j := range joins {
		out = append(out, j)
	}
	sortStrings(out)
	return "\n" + strings.Join(out, "\n")
}

func sortedFilterDims(filters map[string][]string) []string {
	out := make([]string, 0, len(filters))
	for d := range filters {
		out = append(out, d)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
