//go:build cgo

package sqlaudit

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type Report struct {
	Total         int
	Clean         int
	Failed        []Finding
	Exempted      []Finding
	Exemptions    []Exemption
	Unplannable   int // queries the auditor could not analyse — reported, never skipped
	Stale         []Exemption
	AdvisorActive bool
}

func (r Report) Census() string {
	perm, debt := 0, 0
	for _, e := range r.Exemptions {
		if e.Class == "PERMANENT" {
			perm++
		} else {
			debt++
		}
	}
	return fmt.Sprintf("sqlaudit: %d queries — %d clean, %d exempted (%d PERMANENT / %d DEBT), %d failing, %d unanalysable (index_advisor: %v)",
		r.Total, r.Clean, len(r.Exempted), perm, debt, len(r.Failed), r.Unplannable, r.AdvisorActive)
}

func (r Report) String() string {
	var b strings.Builder
	b.WriteString(r.Census() + "\n")
	for _, f := range r.Failed {
		fmt.Fprintf(&b, "  FAIL  %s\n", f)
	}
	for _, e := range r.Stale {
		fmt.Fprintf(&b, "  STALE %s %s %s (line %d) — no longer trips; delete the allowlist line\n",
			e.Class, e.Rule, e.Subject, e.Line)
	}
	return b.String()
}

func (r Report) OK() bool { return len(r.Failed) == 0 && len(r.Stale) == 0 }

// Run audits every query against the vet database. conn must already be through
// PrepareSession; advisor is an optional privileged connection used only to
// enrich failures with a concrete CREATE INDEX suggestion.
func Run(ctx context.Context, conn *pgx.Conn, advisor *pgx.Conn, queries []Query, cat *Catalog, allow *Allowlist) (Report, error) {
	rep := Report{Total: len(queries), AdvisorActive: advisor != nil}
	for _, q := range queries {
		st, err := q.Parse()
		if err != nil {
			rep.unplannable(q, "parse: "+err.Error(), allow)
			continue
		}
		findings := structuralFindings(q, st, cat)

		plan, perr := GenericPlan(ctx, conn, q.SQL)
		if perr != nil {
			rep.unplannable(q, "EXPLAIN GENERIC_PLAN: "+perr.Error(), allow)
		} else {
			findings = append(findings, planFindings(q, st, plan, cat)...)
		}

		if len(findings) == 0 {
			rep.Clean++
			continue
		}
		for _, f := range findings {
			if e, ok := allow.exempt(f); ok {
				rep.Exempted = append(rep.Exempted, f)
				rep.Exemptions = append(rep.Exemptions, e)
				continue
			}
			if advisor != nil {
				f.Suggest = suggestIndex(ctx, advisor, q.SQL)
			}
			rep.Failed = append(rep.Failed, f)
		}
	}
	rep.Stale = allow.Stale()
	sort.Slice(rep.Failed, func(i, j int) bool { return rep.Failed[i].Query < rep.Failed[j].Query })
	sort.Slice(rep.Stale, func(i, j int) bool { return rep.Stale[i].Subject < rep.Stale[j].Subject })
	return rep, nil
}

// unplannable records a query the auditor could not analyse. It is a finding
// like any other — allowlistable, but never silently skipped.
func (r *Report) unplannable(q Query, detail string, allow *Allowlist) {
	f := Finding{Query: q.Name, File: q.File, Rule: RuleUnplannable, Detail: detail}
	r.Unplannable++
	if e, ok := allow.exempt(f); ok {
		r.Exempted = append(r.Exempted, f)
		r.Exemptions = append(r.Exemptions, e)
		return
	}
	r.Failed = append(r.Failed, f)
}

// AdvisorConn returns a connection with supabase/index_advisor available, or
// nil when the caller has not enabled it or the extension is not installed.
//
// OPT-IN ON PURPOSE, off in CI. index_advisor cannot gate: without statistics
// it recommended an index already covered by a unique constraint on a ~1.5%
// cost delta. It is useful once plan shape has ALREADY proven a problem, to
// name the column list — advice, never a verdict. See EXEMPTIONS.md.
func AdvisorConn(ctx context.Context, url string, enabled bool) *pgx.Conn {
	if !enabled {
		return nil
	}
	cfg, err := pgx.ParseConfig(url)
	if err != nil {
		return nil
	}
	// index_advisor DEALLOCATEs inside its own body, which invalidates pgx's
	// statement cache mid-session. Unnamed statements sidestep it.
	cfg.DefaultQueryExecMode = pgx.QueryExecModeExec
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM pg_extension WHERE extname = 'index_advisor'`).Scan(&n); err != nil || n == 0 {
		conn.Close(ctx)
		return nil
	}
	return conn
}

func suggestIndex(ctx context.Context, conn *pgx.Conn, sql string) string {
	var stmts []string
	var errs []string
	err := conn.QueryRow(ctx, `SELECT coalesce(index_statements, '{}'), coalesce(errors, '{}') FROM index_advisor($1)`, sql).
		Scan(&stmts, &errs)
	switch {
	case err != nil:
		return "(index_advisor: " + err.Error() + ")"
	case len(stmts) > 0:
		return strings.Join(stmts, "; ")
	case len(errs) > 0:
		return "(index_advisor: " + strings.Join(errs, "; ") + ")"
	}
	return ""
}
