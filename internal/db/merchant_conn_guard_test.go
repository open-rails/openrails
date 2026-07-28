package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoPoolInsideRunInMerchantConn guards the #826 bug class: a
// RunInMerchantConn callback querying the raw pool (Pool()/DataPool()) instead
// of Qx(ctx). The raw pool has no merchant GUC — under the RLS-enforced
// openrails_app role such queries see ZERO rows fail-closed — and skips the
// #471 schema rewrite. Lexical AST check: any zero-arg .Pool()/.DataPool()
// call inside a function literal passed to RunInMerchantConn fails.
func TestNoPoolInsideRunInMerchantConn(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var violations []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "vendor", ".git", ".claude", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // non-compiling scratch files are not this test's business
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "RunInMerchantConn" {
				return true
			}
			for _, arg := range call.Args {
				fl, ok := arg.(*ast.FuncLit)
				if !ok {
					continue
				}
				ast.Inspect(fl.Body, func(m ast.Node) bool {
					c, ok := m.(*ast.CallExpr)
					if !ok {
						return true
					}
					s, ok := c.Fun.(*ast.SelectorExpr)
					if ok && (s.Sel.Name == "Pool" || s.Sel.Name == "DataPool") && len(c.Args) == 0 {
						violations = append(violations, fset.Position(c.Pos()).String())
					}
					return true
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(violations) > 0 {
		t.Fatalf("raw pool used inside RunInMerchantConn bodies (use Qx(ctx) — the raw pool has no merchant GUC, RLS returns zero rows fail-closed, and it skips the schema rewrite):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}
