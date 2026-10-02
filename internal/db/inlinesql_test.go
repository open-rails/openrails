package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// SQL belongs in internal/db/queries (sqlc). These paths (a trailing slash
// covers a directory) may still hold SQL string literals, each for the reason
// given; an entry that no longer holds any fails as stale.
var inlineSQLAllowed = map[string]string{
	"internal/db/commit_guard.go":         "transaction isolation control",
	"internal/db/sqlaudit/":               "auditor introspects pg_catalog",
	"internal/merchantarchive/archive.go": "profile-driven per-table SQL",
	"internal/merchantarchive/checks.go":  "pg_catalog and per-table checks",
	"internal/merchants/delete.go":        "information_schema table probe",
	"internal/migrate/migrator.go":        "River schema bootstrap, pre-schema",
	"internal/modules/metrics/":           "SQL compiled from metric definitions",
	"internal/river/job_liveness.go":      "River table, runtime schema",
	"internal/river/job_rescue.go":        "River table, runtime schema",
	"internal/river/progress.go":          "River table, runtime schema",
	"sdk/":                                "e2e harness, separate module",
}

var (
	sqlLead      = regexp.MustCompile(`^\s*(--[^\n]*\n\s*)*(SELECT|INSERT|UPDATE|DELETE|WITH|CREATE|ALTER|DROP|GRANT|REVOKE|TRUNCATE|LOCK|SET|COPY|CALL|EXPLAIN)\b`)
	sqlBody      = regexp.MustCompile(`(?i)\b(FROM|INTO|WHERE|TABLE|SCHEMA|ROLE|VALUES|RETURNING|ON CONFLICT|SET\s+\w+\s*=|LOCAL|ISOLATION|TRANSACTION|FUNCTION|SEARCH_PATH|SELECT|ADVISORY|pg_|\w+\()`)
	sqlQualified = regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+(openrails|billing)\.[a-z_]+`)
)

func looksLikeSQL(s string) bool {
	if sqlQualified.MatchString(s) {
		return true
	}
	lead := sqlLead.FindString(s)
	return lead != "" && sqlBody.MatchString(s[len(lead):])
}

// firstInlineSQL returns the line of the first SQL literal in a Go file, or 0.
// Concatenations are judged whole, with non-literal operands as placeholders.
func firstInlineSQL(fset *token.FileSet, file *ast.File) int {
	line := 0
	var visit func(ast.Node) bool
	visit = func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if !ok || line != 0 {
			return line == 0
		}
		var text strings.Builder
		var rest []ast.Expr
		var flatten func(ast.Expr) bool
		flatten = func(e ast.Expr) bool {
			switch x := e.(type) {
			case *ast.BinaryExpr:
				if x.Op == token.ADD {
					return flatten(x.X) && flatten(x.Y)
				}
			case *ast.BasicLit:
				if s, err := strconv.Unquote(x.Value); x.Kind == token.STRING && err == nil {
					text.WriteString(s)
					return true
				}
			}
			if e == expr {
				return false // not a string expression: keep descending
			}
			text.WriteString(" ? ")
			rest = append(rest, e)
			return true
		}
		if !flatten(expr) {
			return true
		}
		if looksLikeSQL(text.String()) {
			line = fset.Position(n.Pos()).Line
		}
		for _, e := range rest {
			ast.Inspect(e, visit)
		}
		return false
	}
	ast.Inspect(file, visit)
	return line
}

func inlineSQLAllowance(path string) (string, bool) {
	for entry := range inlineSQLAllowed {
		if path == entry || (strings.HasSuffix(entry, "/") && strings.HasPrefix(path, entry)) {
			return entry, true
		}
	}
	return "", false
}

func TestNoInlineSQL(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	var violations []string
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if name := d.Name(); rel != "." && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || rel == "internal/db/gen") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		line := firstInlineSQL(fset, file)
		if line == 0 {
			return nil
		}
		if entry, ok := inlineSQLAllowance(rel); ok {
			used[entry] = true
		} else {
			violations = append(violations, rel+":"+strconv.Itoa(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for entry := range inlineSQLAllowed {
		if !used[entry] {
			violations = append(violations, entry+": stale allowance, no inline SQL left")
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("inline SQL outside internal/db/queries; move it to a sqlc query, or allow the file in inlineSQLAllowed with its reason:\n  %s", strings.Join(violations, "\n  "))
	}
}
