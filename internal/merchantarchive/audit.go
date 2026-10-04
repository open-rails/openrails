package merchantarchive

import (
	"fmt"

	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

// AuditStatement is one dynamic archive statement, for the SQL query auditor.
type AuditStatement struct {
	Name string
	SQL  string
}

// AuditStatements lists every statement the archive builds at run time: the
// per-table export and count, and each preflight and reference refusal.
func AuditStatements() []AuditStatement {
	var out []AuditStatement
	for _, p := range contract.Profiles {
		out = append(out,
			AuditStatement{Name: "archive.export." + p.Name, SQL: exportQuery(p)},
			AuditStatement{Name: "archive.count." + p.Name, SQL: countQuery(p)})
	}
	for i, c := range preflightChecks {
		out = append(out, AuditStatement{Name: fmt.Sprintf("archive.preflight.%s.%d", c.table, i), SQL: refuseQuery(c.table, c.predicate)})
	}
	for i, c := range referenceChecks {
		out = append(out, AuditStatement{Name: fmt.Sprintf("archive.reference.%s.%d", c.table, i), SQL: refuseQuery(c.table, c.predicate)})
	}
	return out
}
