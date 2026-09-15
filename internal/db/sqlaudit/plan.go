//go:build cgo

package sqlaudit

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// AuditMerchantID is an arbitrary UUID for the app.merchant_id GUC. RLS
// predicates plan on the shape of the setting, never on its value.
const AuditMerchantID = "00000000-0000-0000-0000-0000000a1d17"

// planNode is the subset of EXPLAIN FORMAT JSON the rules read.
type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	Schema       string     `json:"Schema"`
	Alias        string     `json:"Alias"`
	IndexName    string     `json:"Index Name"`
	IndexCond    string     `json:"Index Cond"`
	RecheckCond  string     `json:"Recheck Cond"`
	Filter       string     `json:"Filter"`
	Plans        []planNode `json:"Plans"`
}

type explainRoot struct {
	Plan planNode `json:"Plan"`
}

func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// PrepareSession puts the connection in the state production actually runs in:
// the unprivileged openrails_app role with the merchant GUC set, so RLS
// predicates appear in the plan.
//
// This probes index availability, not production plan cost. Empty-table row
// width estimates can favor a sequential EXISTS scan despite a usable index.
// Discourage that choice without fabricating statistics; planFindings still
// rejects a forced sequential scan or an index scan with only a residual filter.
// Actual workload performance is covered by the populated query/perf harness.
func PrepareSession(ctx context.Context, conn *pgx.Conn) error {
	stmts := []string{
		`SET ROLE openrails_app`,
		`SELECT set_config('app.merchant_id', '` + AuditMerchantID + `', false)`,
		`SET search_path = openrails, public`,
		`SET enable_seqscan = off`,
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return fmt.Errorf("session setup %q: %w", s, err)
		}
	}
	return nil
}

// GenericPlan runs EXPLAIN (GENERIC_PLAN) — PG16+ plans a parameterized
// statement without values, so no parameters are fabricated. EXPLAIN without
// ANALYZE never executes, so DML is planned but never run.
func GenericPlan(ctx context.Context, conn *pgx.Conn, sql string) (planNode, error) {
	// Raw simple-query protocol: the $n placeholders belong to the statement
	// being EXPLAINed, not to the EXPLAIN itself, so pgx must not bind or
	// interpolate them (QueryExecModeSimpleProtocol still interpolates and
	// fails with "insufficient arguments"). Because this is raw text, the
	// caller must first prove the string is exactly ONE statement — see
	// singleStatement — so `EXPLAIN a; b` can never smuggle b past EXPLAIN.
	res := conn.PgConn().Exec(ctx, "EXPLAIN (GENERIC_PLAN, FORMAT JSON, COSTS ON) "+sql)
	results, err := res.ReadAll()
	if err != nil {
		return planNode{}, err
	}
	if len(results) == 0 || len(results[0].Rows) == 0 || len(results[0].Rows[0]) == 0 {
		return planNode{}, fmt.Errorf("empty EXPLAIN output")
	}
	raw := results[0].Rows[0][0]
	var roots []explainRoot
	if err := json.Unmarshal(raw, &roots); err != nil {
		return planNode{}, err
	}
	if len(roots) == 0 {
		return planNode{}, fmt.Errorf("empty EXPLAIN output")
	}
	return roots[0].Plan, nil
}
