package metrics

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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

// compile builds one parameterized statement per source family involved.
// Every SQL fragment comes from the registry; client values ride in args.
func compile(plan *Plan, merchantID uuid.UUID) ([]stmt, error) {
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
		spec := families[fam]
		var st stmt
		var err error
		switch spec.Kind {
		case "flow":
			st, err = compileFlow(plan, merchantID, fam, spec, leaves)
		case "snapshot":
			st, err = compileSnapshot(plan, merchantID, fam, spec, leaves)
		case "balance":
			st, err = compileBalance(plan, merchantID, fam, spec, leaves)
		case "depletion":
			st, err = compileDepletion(plan, merchantID, leaves)
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

// merchantCols: explicit merchant predicate per family (defense in depth on top
// of RLS, and it anchors the (merchant_id, time) indexes).
var merchantCols = map[Family]string{
	FamPayments:       "p.merchant_id",
	FamSubsNew:        "s.merchant_id",
	FamSubsCancelled:  "s.merchant_id",
	FamSubsEnded:      "s.merchant_id",
	FamGrants:         "g.merchant_id",
	FamUsage:          "ue.merchant_id",
	FamTransitions:    "st.merchant_id",
	FamDenials:        "ad.merchant_id",
	FamSubsSnapshot:   "s.merchant_id",
	FamEntitlSnapshot: "e.merchant_id",
	FamBalance:        "lt.merchant_id",
}

func compileFlow(plan *Plan, merchantID uuid.UUID, fam Family, spec familySpec, leaves []*Measure) (stmt, error) {
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	sel := []string{}
	group := []string{}
	if plan.HasTime {
		sel = append(sel, fmt.Sprintf("date_trunc(%s, %s AT TIME ZONE 'UTC') AS bucket", arg(plan.Grain), spec.TimeExpr))
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
	where = append(where,
		fmt.Sprintf("%s >= %s", spec.TimeExpr, arg(plan.From)),
		fmt.Sprintf("%s < %s", spec.TimeExpr, arg(plan.To)))
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

func compileBalance(plan *Plan, merchantID uuid.UUID, fam Family, spec familySpec, leaves []*Measure) (stmt, error) {
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	sel := []string{}
	group := []string{}
	firstEdge, lastEdge := plan.To, plan.To // non-time: everything strictly before range end
	if plan.HasTime {
		firstEdge, lastEdge = plan.Buckets[0], plan.Buckets[len(plan.Buckets)-1]
		sel = append(sel, fmt.Sprintf(
			"CASE WHEN %s < %s THEN NULL ELSE date_trunc(%s, %s AT TIME ZONE 'UTC') END AS bucket",
			spec.TimeExpr, arg(firstEdge), arg(plan.Grain), spec.TimeExpr))
		group = append(group, "1")
	}
	for _, d := range plan.Dims {
		expr, ok := spec.DimExprs[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: family %s lacks dimension %s (registry bug)", fam, d)
		}
		sel = append(sel, expr+" AS dim_"+d)
		group = append(group, fmt.Sprintf("%d", len(sel)))
	}
	for _, m := range leaves {
		sel = append(sel, m.Expr+" AS m_"+m.Name)
	}

	where := []string{merchantCols[fam] + " = " + arg(merchantID)}
	if plan.HasTime {
		where = append(where, fmt.Sprintf("%s < %s", spec.TimeExpr, arg(lastEdge)))
	} else {
		where = append(where, fmt.Sprintf("%s < %s", spec.TimeExpr, arg(plan.To)))
	}
	for _, d := range sortedFilterDims(plan.Filters) {
		expr, ok := spec.DimExprs[d]
		if !ok {
			return stmt{}, fmt.Errorf("metrics: family %s lacks filter dimension %s (registry bug)", fam, d)
		}
		where = append(where, fmt.Sprintf("%s = ANY(%s)", expr, arg(plan.Filters[d])))
	}

	sql := "SELECT " + strings.Join(sel, ",\n       ") +
		"\nFROM " + spec.From +
		"\nWHERE " + strings.Join(where, "\n  AND ")
	if len(group) > 0 {
		sql += "\nGROUP BY " + strings.Join(group, ", ")
	}
	return stmt{family: fam, sql: sql, args: args, leaves: leaves, hasTime: plan.HasTime, balance: true}, nil
}

// compileDepletion: payers whose prepaid balance at the edge covers <= N days
// of their trailing-7d burn, in any currency. Bounded: edges x payers.
func compileDepletion(plan *Plan, merchantID uuid.UUID, leaves []*Measure) (stmt, error) {
	edges := snapshotEdges(plan, leaves)
	sql := fmt.Sprintf(`SELECT edge.bucket AS bucket, COUNT(DISTINCT x.customer_id) AS m_payers_at_depletion_risk
FROM unnest($2::timestamptz[]) AS edge(bucket)
LEFT JOIN LATERAL (
  SELECT bal.customer_id
  FROM (
    SELECT la.customer_id, la.currency,
           SUM(CASE WHEN lt.credit_account_id = la.id THEN lt.amount ELSE -lt.amount END) AS balance
    FROM openrails.ledger_accounts la
    JOIN openrails.ledger_transfers lt
      ON (lt.credit_account_id = la.id OR lt.debit_account_id = la.id)
     AND lt.created_at < edge.bucket
    WHERE la.merchant_id = $1 AND la.account_type = 'customer_balance' AND la.customer_id IS NOT NULL
    GROUP BY la.customer_id, la.currency
  ) bal
  JOIN (
    SELECT ue.customer_id, ue.currency, SUM(ue.amount) AS burn
    FROM openrails.usage_events ue
    WHERE ue.merchant_id = $1 AND ue.occurred_at > edge.bucket - interval '7 days' AND ue.occurred_at <= edge.bucket
    GROUP BY ue.customer_id, ue.currency
  ) burn ON burn.customer_id = bal.customer_id AND burn.currency = bal.currency
  WHERE burn.burn > 0 AND bal.balance <= (burn.burn / 7.0) * %d
) x ON TRUE
GROUP BY edge.bucket`, depletionRiskDays)
	return stmt{
		family: FamDepletion, sql: sql, args: []any{merchantID, edges},
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
