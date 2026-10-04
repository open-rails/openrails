package metrics

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// AuditStatement is one compiled statement shape, for the SQL query auditor.
type AuditStatement struct {
	Name string
	SQL  string
}

// AuditStatements compiles every family twice, bucketed by day and grouped by
// every dimension it supports, and as one ungrouped total, so the auditor plans
// the joins and predicates the service can emit.
func AuditStatements() ([]AuditStatement, error) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	fams := make([]string, 0, len(families))
	for fam := range families {
		fams = append(fams, string(fam))
	}
	sort.Strings(fams)
	var out []AuditStatement
	for _, name := range fams {
		fam := Family(name)
		var leaves []*Measure
		for i := range Measures {
			if Measures[i].Family == fam && Measures[i].Class != ClassRatio {
				leaves = append(leaves, &Measures[i])
			}
		}
		if len(leaves) == 0 {
			continue
		}
		dims := make([]string, 0, len(families[fam].DimExprs))
		for d := range families[fam].DimExprs {
			dims = append(dims, d)
		}
		sort.Strings(dims)
		for _, v := range []struct {
			suffix string
			plan   Plan
		}{
			{"by_day", Plan{Measures: leaves, Dims: dims, HasTime: true, Grain: "day", From: from, To: to, Buckets: []time.Time{from}}},
			{"total", Plan{Measures: leaves, From: from, To: to}},
		} {
			stmts, err := compile(&v.plan, uuid.Nil)
			if err != nil {
				return nil, err
			}
			for i, st := range stmts {
				n := "metrics." + name + "." + v.suffix
				if i > 0 {
					n += "." + string(rune('a'+i))
				}
				out = append(out, AuditStatement{Name: n, SQL: st.sql})
			}
		}
	}
	return out, nil
}
