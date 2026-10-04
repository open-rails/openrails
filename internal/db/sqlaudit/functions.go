//go:build cgo

package sqlaudit

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	pgq "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// LoadFunctionQueries extracts the statements stored functions and triggers
// run, so they are planned like any sqlc query. NEW/OLD fields, variables,
// arguments and trigger context become typed parameters; a trigger function is
// audited once per table it is attached to. Statements that touch no billing
// table (pure expressions, dynamic EXECUTE, catalog reads) are not queries.
func LoadFunctionQueries(ctx context.Context, conn *pgx.Conn) ([]Query, error) {
	columns, err := loadColumnTypes(ctx, conn)
	if err != nil {
		return nil, err
	}
	rows, err := conn.Query(ctx, `
SELECT p.proname, l.lanname, pg_get_functiondef(p.oid), p.prosrc,
       COALESCE(p.proargnames, '{}'),
       COALESCE((SELECT array_agg(format_type(t, NULL) ORDER BY o) FROM unnest(p.proargtypes) WITH ORDINALITY u(t, o)), '{}'),
       COALESCE((SELECT array_agg(DISTINCT c.relname ORDER BY c.relname) FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid
                  WHERE tg.tgfoid = p.oid AND NOT tg.tgisinternal), '{}')
  FROM pg_proc p
  JOIN pg_namespace n ON n.oid = p.pronamespace
  JOIN pg_language l ON l.oid = p.prolang
 WHERE n.nspname = 'billing'
 ORDER BY p.proname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Query
	for rows.Next() {
		var name, lang, def, src string
		var argNames, argTypes, tables []string
		if err := rows.Scan(&name, &lang, &def, &src, &argNames, &argTypes, &tables); err != nil {
			return nil, err
		}
		fn := function{name: name, columns: columns, vars: map[string]string{}, records: map[string]string{}}
		for i, a := range argNames {
			if a != "" && i < len(argTypes) {
				fn.vars[a] = argTypes[i]
			}
		}
		var stmts []string
		switch lang {
		case "sql":
			stmts = []string{src}
		case "plpgsql":
			stmts, err = fn.plpgsql(def)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
		default:
			continue
		}
		if len(tables) == 0 {
			tables = []string{""}
		}
		for _, table := range tables {
			fn.trigger = table
			for i, s := range stmts {
				sql, ok, err := fn.bind(s)
				label := fmt.Sprintf("function.%s.%d", name, i)
				if table != "" {
					label = fmt.Sprintf("trigger.%s.%s.%d", name, table, i)
				}
				switch {
				case err != nil:
					out = append(out, Query{Name: label, Kind: "function", SQL: s, File: "0001_schema.up.sql", bindErr: err})
				case ok:
					out = append(out, Query{Name: label, Kind: "function", SQL: sql, File: "0001_schema.up.sql"})
				}
			}
		}
	}
	return out, rows.Err()
}

func loadColumnTypes(ctx context.Context, conn *pgx.Conn) (map[string]map[string]string, error) {
	rows, err := conn.Query(ctx, `
SELECT c.relname, a.attname, format_type(a.atttypid, a.atttypmod)
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
 WHERE n.nspname = 'billing' AND c.relkind IN ('r', 'p')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var table, col, typ string
		if err := rows.Scan(&table, &col, &typ); err != nil {
			return nil, err
		}
		if out[table] == nil {
			out[table] = map[string]string{}
		}
		out[table][col] = typ
	}
	return out, rows.Err()
}

type function struct {
	name    string
	trigger string                       // the table a trigger function is audited for
	columns map[string]map[string]string // billing table -> column -> type
	vars    map[string]string            // variable or argument -> type
	records map[string]string            // %ROWTYPE variable -> billing table
}

// plpgsql returns the SQL text of every statement and expression in the body.
func (f *function) plpgsql(def string) ([]string, error) {
	raw, err := pgq.ParsePlPgSqlToJSON(def)
	if err != nil {
		return nil, err
	}
	var tree any
	if err := json.Unmarshal([]byte(raw), &tree); err != nil {
		return nil, err
	}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			if d, ok := x["PLpgSQL_var"].(map[string]any); ok {
				name, _ := d["refname"].(string)
				typ := ""
				if dt, ok := d["datatype"].(map[string]any); ok {
					if t, ok := dt["PLpgSQL_type"].(map[string]any); ok {
						typ, _ = t["typname"].(string)
					}
				}
				if name != "" && typ != "" {
					if table, ok := strings.CutSuffix(typ, "%ROWTYPE"); ok {
						f.records[name] = strings.TrimPrefix(table, "billing.")
					} else {
						f.vars[name] = typ
					}
				}
			}
			if e, ok := x["PLpgSQL_expr"].(map[string]any); ok {
				q, _ := e["query"].(string)
				mode, _ := e["parseMode"].(float64)
				switch {
				case mode == 0: // a statement
					out = append(out, q)
				case mode >= 3: // an assignment: audit its right-hand side
					if _, rhs, ok := strings.Cut(q, ":="); ok {
						out = append(out, "SELECT "+rhs)
					}
				default: // an expression
					out = append(out, "SELECT "+q)
				}
			}
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(x[k])
			}
		}
	}
	walk(tree)
	return out, nil
}

// bind replaces plpgsql references with typed parameters. ok is false when the
// statement reads no billing table.
func (f *function) bind(sql string) (string, bool, error) {
	if !strings.Contains(sql, "billing.") {
		return "", false, nil
	}
	tree, err := pgq.Parse(sql)
	if err != nil {
		return "", false, err
	}
	if len(tree.Stmts) != 1 {
		return "", false, fmt.Errorf("expected one statement, parsed %d", len(tree.Stmts))
	}
	touches := false
	params := map[string]string{} // placeholder -> typed parameter
	var bindErr error
	next := 0
	placeholder := func(typ string) string {
		next++
		p := fmt.Sprintf("plpgsql_param_%d", next)
		params[p] = fmt.Sprintf("$%d::%s", next, typ)
		return p
	}
	var visit func(protoreflect.Message)
	visit = func(m protoreflect.Message) {
		if rv, ok := m.Interface().(*pgq.RangeVar); ok && rv.Schemaname == "billing" {
			touches = true
		}
		if n, ok := m.Interface().(*pgq.Node); ok {
			if ref := n.GetColumnRef(); ref != nil {
				if typ, ok := f.refType(ref); ok {
					n.Node = &pgq.Node_ColumnRef{ColumnRef: &pgq.ColumnRef{Fields: []*pgq.Node{pgq.MakeStrNode(placeholder(typ))}}}
					return
				} else if typ == "?" && bindErr == nil {
					bindErr = fmt.Errorf("cannot type %s", columnPathOf(ref))
				}
			}
		}
		m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
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
	visit(tree.ProtoReflect())
	if !touches {
		return "", false, nil
	}
	if bindErr != nil {
		return "", false, bindErr
	}
	out, err := pgq.Deparse(tree)
	if err != nil {
		return "", false, err
	}
	// Longest placeholder first, so plpgsql_param_1 never rewrites inside _10.
	names := make([]string, 0, len(params))
	for p := range params {
		names = append(names, p)
	}
	sort.Slice(names, func(i, j int) bool {
		return len(names[i]) > len(names[j]) || len(names[i]) == len(names[j]) && names[i] > names[j]
	})
	for _, p := range names {
		out = strings.ReplaceAll(out, p, params[p])
	}
	return out, true, nil
}

// refType is the parameter type standing in for a plpgsql reference. "?" means
// the reference is plpgsql-owned but its type is unknown; ok=false otherwise
// (a real column, left alone).
func (f *function) refType(ref *pgq.ColumnRef) (string, bool) {
	var parts []string
	for _, fld := range ref.Fields {
		s := fld.GetString_()
		if s == nil {
			return "", false
		}
		parts = append(parts, s.GetSval())
	}
	head := strings.ToLower(parts[0])
	switch {
	case head == "tg_op" || head == "tg_table_name" || head == "tg_table_schema" || head == "tg_name":
		return "text", len(parts) == 1
	case head == "tg_argv":
		return "text[]", len(parts) == 1
	case head == "found":
		return "boolean", len(parts) == 1
	case head == "new" || head == "old":
		if f.trigger == "" {
			return "?", false
		}
		if len(parts) == 1 {
			return "billing." + f.trigger, true
		}
		if typ, ok := f.columns[f.trigger][parts[1]]; ok && len(parts) == 2 {
			return typ, true
		}
		return "?", false
	}
	if table, ok := f.records[parts[0]]; ok {
		if len(parts) == 2 {
			if typ, ok := f.columns[table][parts[1]]; ok {
				return typ, true
			}
		}
		return "?", false
	}
	if typ, ok := f.vars[parts[0]]; ok && len(parts) == 1 {
		return typ, true
	}
	return "", false
}

func columnPathOf(ref *pgq.ColumnRef) string {
	var parts []string
	for _, fld := range ref.Fields {
		parts = append(parts, fld.GetString_().GetSval())
	}
	return strings.Join(parts, ".")
}
