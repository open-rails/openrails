//go:build cgo

// Package sqlaudit audits every sqlc query for efficiency and safety, not just
// correctness. `sqlc vet` (db-prepare) proves a query is valid SQL; this proves
// it does not scan without bound. See internal/db/queries/EXEMPTIONS.md.
package sqlaudit

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	pgq "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Query is one sqlc query as it is actually sent to Postgres: the generated
// const text, with $n placeholders already substituted.
type Query struct {
	Name string // e.g. ListDueDunningSubscriptions
	Kind string // one/many/exec/execrows/copyfrom/batch*
	SQL  string
	File string
}

var (
	constStart = regexp.MustCompile("^const [a-zA-Z0-9_]+ = `(-- name: ([A-Za-z0-9_]+) :([a-z]+))$")
	// sqlc emits `/*SLICE:name*/$1`; pgx expands it at call time. One
	// placeholder plans to the same shape.
	sliceMarker = regexp.MustCompile(`/\*SLICE:[a-zA-Z0-9_]+\*/`)
)

// LoadQueries parses generated internal/db/gen/*.sql.go for query consts.
// Reading the generated text (not the .sql sources) means the auditor sees the
// exact string the pool executes — sqlc.arg/narg/embed already resolved.
func LoadQueries(genDir string) ([]Query, error) {
	files, err := filepath.Glob(filepath.Join(genDir, "*.sql.go"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Query
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		lines := strings.Split(string(b), "\n")
		for i := 0; i < len(lines); i++ {
			m := constStart.FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			var body []string
			for j := i + 1; j < len(lines); j++ {
				if strings.HasPrefix(lines[j], "`") {
					i = j
					break
				}
				body = append(body, lines[j])
			}
			out = append(out, Query{
				Name: m[2],
				Kind: m[3],
				SQL:  sliceMarker.ReplaceAllString(strings.Join(body, "\n"), ""),
				File: filepath.Base(f),
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no queries found in %s", genDir)
	}
	return out, nil
}

// Structure is what the real Postgres parser (pg_query_go) says about a query.
// No regex: the AST is authoritative about LIMIT, statement kind, and which
// columns a WHERE clause actually pins.
type Structure struct {
	Writes      bool                // UPDATE/DELETE somewhere in the statement
	HasLimit    bool                // any SELECT level carries LIMIT
	Relations   []string            // every table referenced
	WriteTables []string            // tables an UPDATE/DELETE targets
	EqParams    map[string]struct{} // columns pinned by `col = $n` (parameter, not constant)
	AnyParams   map[string]struct{} // columns pinned by `col = ANY($n)` (caller-supplied list)
	WhereCols   map[string]struct{} // every column referenced anywhere in a WHERE
}

// Parse walks the Postgres parse tree. Bounding evidence is deliberately narrow:
// only `col = $n` counts. `status = 'past_due'` pins a bucket, not a row set,
// and `col = ANY($1)` is set membership over a low-cardinality enum.
func (q Query) Parse() (*Structure, error) {
	tree, err := pgq.Parse(q.SQL)
	if err != nil {
		return nil, err
	}
	// The auditor EXPLAINs this text over the raw simple-query protocol, which
	// would happily run a second statement. sqlc queries are single statements
	// by construction; anything else is refused, not audited.
	if len(tree.Stmts) != 1 {
		return nil, fmt.Errorf("expected exactly 1 statement, parsed %d", len(tree.Stmts))
	}
	s := &Structure{
		EqParams:  map[string]struct{}{},
		AnyParams: map[string]struct{}{},
		WhereCols: map[string]struct{}{},
	}
	for _, raw := range tree.Stmts {
		walk(raw.Stmt.ProtoReflect(), s)
	}
	sort.Strings(s.Relations)
	sort.Strings(s.WriteTables)
	return s, nil
}

// walk recurses the parse tree generically via protoreflect, so no node kind is
// silently missed (CTEs, sublinks, set operations all get visited).
func walk(m protoreflect.Message, s *Structure) {
	switch v := m.Interface().(type) {
	case *pgq.UpdateStmt:
		s.Writes = true
		s.WriteTables = append(s.WriteTables, v.Relation.GetRelname())
		collectWhere(v.WhereClause, s)
	case *pgq.DeleteStmt:
		s.Writes = true
		s.WriteTables = append(s.WriteTables, v.Relation.GetRelname())
		collectWhere(v.WhereClause, s)
	case *pgq.SelectStmt:
		if v.LimitCount != nil {
			s.HasLimit = true
		}
		collectWhere(v.WhereClause, s)
	case *pgq.RangeVar:
		s.Relations = append(s.Relations, v.GetRelname())
	}
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			l := val.List()
			for i := 0; i < l.Len(); i++ {
				walk(l.Get(i).Message(), s)
			}
		case fd.Kind() == protoreflect.MessageKind:
			walk(val.Message(), s)
		}
		return true
	})
}

// collectWhere records the columns a WHERE clause touches and, separately, the
// ones it pins to a query parameter.
func collectWhere(where *pgq.Node, s *Structure) {
	if where == nil {
		return
	}
	var rec func(n *pgq.Node)
	rec = func(n *pgq.Node) {
		if n == nil {
			return
		}
		if e := n.GetAExpr(); e != nil && opName(e) == "=" {
			switch e.Kind {
			case pgq.A_Expr_Kind_AEXPR_OP:
				if c := colName(e.Lexpr); c != "" && isParam(e.Rexpr) {
					s.EqParams[c] = struct{}{}
				}
				if c := colName(e.Rexpr); c != "" && isParam(e.Lexpr) {
					s.EqParams[c] = struct{}{}
				}
			case pgq.A_Expr_Kind_AEXPR_OP_ANY, pgq.A_Expr_Kind_AEXPR_IN:
				// `col = ANY($n)` / `col IN (...)`: the caller's list is the bound.
				if c := colName(e.Lexpr); c != "" && isParam(e.Rexpr) {
					s.AnyParams[c] = struct{}{}
				}
			}
		}
		if c := colName(n); c != "" {
			s.WhereCols[c] = struct{}{}
		}
		walkNodes(n.ProtoReflect(), rec)
	}
	rec(where)
}

func walkNodes(m protoreflect.Message, fn func(*pgq.Node)) {
	m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		visit := func(msg protoreflect.Message) {
			if n, ok := msg.Interface().(*pgq.Node); ok {
				fn(n)
				return
			}
			walkNodes(msg, fn)
		}
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.MessageKind:
			l := val.List()
			for i := 0; i < l.Len(); i++ {
				visit(l.Get(i).Message())
			}
		case fd.Kind() == protoreflect.MessageKind:
			visit(val.Message())
		}
		return true
	})
}

func opName(e *pgq.A_Expr) string {
	if len(e.Name) == 0 {
		return ""
	}
	return e.Name[0].GetString_().GetSval()
}

// colName returns the bare (last) column name of a ColumnRef, e.g. sub.id -> id.
func colName(n *pgq.Node) string {
	cr := n.GetColumnRef()
	if cr == nil || len(cr.Fields) == 0 {
		return ""
	}
	return cr.Fields[len(cr.Fields)-1].GetString_().GetSval()
}

// isParam reports whether a node is a query parameter, possibly cast.
func isParam(n *pgq.Node) bool {
	if n == nil {
		return false
	}
	if n.GetParamRef() != nil {
		return true
	}
	if tc := n.GetTypeCast(); tc != nil {
		return isParam(tc.Arg)
	}
	return false
}
