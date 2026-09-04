//go:build cgo

package sqlaudit

import (
	"fmt"
	"sort"
	"strings"

	pgq "github.com/pganalyze/pg_query_go/v6"
)

// Rule ids. Each catches a distinct failure class; keep them few and sharp.
// Names are shared with tensorhub's equivalent gate so allowlists stay portable.
const (
	// RuleUnboundedMany: a :many query whose result set is bounded by nothing
	// but the merchant. Row count grows with records on file, not with activity.
	RuleUnboundedMany = "unbounded-many"
	// RuleUnscopedWrite: UPDATE/DELETE that pins neither merchant_id nor a key.
	// Under a merchant-scoped pool RLS saves it; a worker pool that bypasses
	// RLS rewrites the deployment.
	RuleUnscopedWrite = "unscoped-write"
	// RuleSeqScan: the planner had no usable index for any predicate.
	RuleSeqScan = "seq-scan"
	// RuleUnplannable: EXPLAIN or the parser could not analyse the query. Never
	// silently skipped — it fails like any other finding.
	RuleUnplannable = "unplannable"
	// RuleUnindexedFilter: the query looks something up by `col = $n`, the scan
	// is narrowed by nothing but merchant_id, and no index on that table covers
	// col. This is what a missing index looks like UNDER RLS, where the
	// merchant_id index always hands the planner some index path so the miss
	// never surfaces as a Seq Scan. Catalog truth, not planner choice: a column
	// indexed by any other index is never flagged. (openrails-only — tensorhub
	// has no RLS, so it has no equivalent.)
	RuleUnindexedFilter = "unindexed-filter"
)

type Finding struct {
	Query  string
	File   string
	Rule   string
	Detail string
	// Suggest is index_advisor's recommendation, when the extension is present.
	Suggest string
}

func (f Finding) String() string {
	s := fmt.Sprintf("%s (%s) [%s]: %s", f.Query, f.File, f.Rule, f.Detail)
	if f.Suggest != "" {
		s += "\n      index_advisor suggests: " + f.Suggest
	}
	return s
}

// boundingCols is the set of predicates that actually bound a result set.
// merchant_id never counts: every row of one merchant still grows with records
// on file. `col = $n` counts when index-backed; `col = ANY($n)` counts only on
// a UNIQUE column, where the caller's list caps the row count — on a non-unique
// column (price_id, customer_id) each element can fan out without limit.
func boundingCols(s *Structure, cat *Catalog) []string {
	var out []string
	for c := range s.EqParams {
		if c != "merchant_id" && cat.indexedAnywhere(c, s.Relations) {
			out = append(out, c)
		}
	}
	for c := range s.AnyParams {
		if c != "merchant_id" && cat.capsRowCount(c, s, s.Relations) {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out
}

// structuralFindings applies the parser-based rules (planner-independent).
func structuralFindings(q Query, s *Structure, cat *Catalog) []Finding {
	var out []Finding
	scoped := cat.anyMerchantScoped(s.Relations)

	if q.Kind == "many" && !s.HasLimit && scoped && len(boundingCols(s, cat)) == 0 {
		out = append(out, Finding{
			Query: q.Name, File: q.File, Rule: RuleUnboundedMany,
			Detail: fmt.Sprintf("reads %s with no LIMIT and no indexed `col = $n` predicate; result grows with records on file",
				strings.Join(scopedTables(s.Relations, cat), ", ")),
		})
	}

	// A LIMITed row source (the `WITH due AS (... LIMIT n FOR UPDATE SKIP
	// LOCKED) UPDATE ...` claim pattern) bounds the write by construction.
	if s.Writes && !s.HasLimit {
		var naked []string
		for _, t := range s.WriteTables {
			if !cat.MerchantScoped[t] {
				continue
			}
			if _, ok := s.WhereCols["merchant_id"]; ok {
				continue
			}
			if len(boundingCols(s, cat)) > 0 {
				continue
			}
			naked = append(naked, t)
		}
		if len(naked) > 0 {
			out = append(out, Finding{
				Query: q.Name, File: q.File, Rule: RuleUnscopedWrite,
				Detail: fmt.Sprintf("writes %s without merchant_id or an indexed key in WHERE", strings.Join(naked, ", ")),
			})
		}
	}
	return out
}

// planFindings applies the planner-based rule to an EXPLAIN plan tree.
func planFindings(q Query, st *Structure, plan planNode, cat *Catalog) []Finding {
	seen := map[string]bool{}
	var out []Finding
	plan.walk(func(n planNode) {
		if n.NodeType != "Seq Scan" || n.RelationName == "" || seen[n.RelationName] {
			return
		}
		if !cat.MerchantScoped[n.RelationName] {
			return // control-plane table (merchants, worker_health, probe_verdicts)
		}
		seen[n.RelationName] = true
		out = append(out, Finding{
			Query: q.Name, File: q.File, Rule: RuleSeqScan,
			Detail: fmt.Sprintf("full scan of %s: no index serves any predicate (filter: %s)",
				n.RelationName, compact(n.Filter)),
		})
	})

	flagged := map[string]bool{}
	plan.walk(func(n planNode) {
		if n.Filter == "" || n.RelationName == "" || flagged[n.RelationName] {
			return
		}
		if !cat.MerchantScoped[n.RelationName] {
			return
		}
		// Only when nothing but the merchant narrows the scan. If the index
		// cond pins a real key, a residual filter discards a handful of rows
		// and is not worth an index.
		if indexCondNarrows(n, cat) || filterPinsPrimaryKey(n, cat) {
			return
		}
		// Only lookup columns (`col = $n` in the source), not incidental
		// residuals: `status = 'active'` or `deleted_at IS NULL` after an index
		// has already narrowed the scan costs nothing and wants no index.
		var bare []string
		for _, col := range cat.Columns[n.RelationName] {
			if col == "merchant_id" || cat.Indexed[n.RelationName+"."+col] {
				continue
			}
			if _, lookup := st.EqParams[col]; lookup && mentions(n.Filter, col) {
				bare = append(bare, col)
			}
		}
		if len(bare) == 0 {
			return
		}
		flagged[n.RelationName] = true
		sort.Strings(bare)
		out = append(out, Finding{
			Query: q.Name, File: q.File, Rule: RuleUnindexedFilter,
			Detail: fmt.Sprintf("%s rows are discarded on %s, which no index on that table covers (filter: %s)",
				n.RelationName, strings.Join(bare, ", "), compact(n.Filter)),
		})
	})
	return out
}

// On the empty vet database PostgreSQL can choose a merchant index and leave
// the addressed primary-key equality in Filter. The existing PK index already
// serves this one-row lookup; CAS guards need no new indexes of their own.
// Recognize only a direct equality guaranteed by AND, on this scan's single
// primary-key column. OR, casts/functions of the column, and composite-key
// fragments cannot establish that bound. Seq-scan findings remain independent.
func filterPinsPrimaryKey(scan planNode, cat *Catalog) bool {
	key := cat.PrimaryKeys[scan.RelationName]
	if len(key) != 1 || !mentions(scan.Filter, key[0]) {
		return false
	}
	tree, err := pgq.Parse("SELECT 1 WHERE " + scan.Filter)
	if err != nil || len(tree.Stmts) != 1 {
		return false
	}
	var pinned func(*pgq.Node) bool
	column := func(node *pgq.Node) bool {
		ref := node.GetColumnRef()
		if ref == nil || colName(node) != key[0] {
			return false
		}
		if len(ref.Fields) == 1 {
			return true
		}
		if len(ref.Fields) != 2 {
			return false
		}
		qualifier := ref.Fields[0].GetString_().GetSval()
		if scan.Alias != "" {
			return qualifier == scan.Alias
		}
		return qualifier == scan.RelationName
	}
	pinned = func(node *pgq.Node) bool {
		if node == nil {
			return false
		}
		if expr := node.GetBoolExpr(); expr != nil {
			if expr.Boolop != pgq.BoolExprType_AND_EXPR {
				return false
			}
			for _, arg := range expr.Args {
				if pinned(arg) {
					return true
				}
			}
			return false
		}
		expr := node.GetAExpr()
		if expr == nil || expr.Kind != pgq.A_Expr_Kind_AEXPR_OP || opName(expr) != "=" {
			return false
		}
		return column(expr.Lexpr) && (isParam(expr.Rexpr) || isConst(expr.Rexpr)) || column(expr.Rexpr) && (isParam(expr.Lexpr) || isConst(expr.Lexpr))
	}
	return pinned(tree.Stmts[0].Stmt.GetSelectStmt().WhereClause)
}

// indexCondNarrows reports whether the scan's index condition pins something
// other than merchant_id — i.e. the index does real work beyond RLS.
func indexCondNarrows(n planNode, cat *Catalog) bool {
	cond := n.IndexCond + " " + n.RecheckCond
	for _, col := range cat.Columns[n.RelationName] {
		if col != "merchant_id" && mentions(cond, col) {
			return true
		}
	}
	return false
}

// mentions reports whether an EXPLAIN predicate string references a column,
// matching on identifier boundaries so `id` does not match `price_id`.
func mentions(filter, col string) bool {
	for i := 0; ; {
		j := strings.Index(filter[i:], col)
		if j < 0 {
			return false
		}
		j += i
		before := byte(' ')
		if j > 0 {
			before = filter[j-1]
		}
		after := byte(' ')
		if j+len(col) < len(filter) {
			after = filter[j+len(col)]
		}
		if !isIdentByte(before) && !isIdentByte(after) {
			return true
		}
		i = j + len(col)
	}
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func scopedTables(rels []string, cat *Catalog) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rels {
		if cat.MerchantScoped[r] && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func compact(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:157] + "..."
	}
	if s == "" {
		return "none"
	}
	return s
}
