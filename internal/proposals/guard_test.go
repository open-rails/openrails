package proposals

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOneProposalExecutor: this package is the only code that applies a
// pull.* change. Locking and resolving a proposal (the queries that mark a
// change approved, dismissed or superseded) and reading a persisted change's
// executable payload happen here and nowhere else, across both modules.
func TestOneProposalExecutor(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected module root at %s: %v", root, err)
	}
	owners := map[string][]string{
		"LockProposal":    {"internal/proposals", "internal/db/gen"},
		"ResolveProposal": {"internal/proposals", "internal/db/gen"},
		"DecodeChange":    {"internal/proposals", "internal/reconcile"},
	}
	var violations []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "vendor" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			allowed, guarded := owners[sel.Sel.Name]
			if !guarded {
				return true
			}
			for _, owner := range allowed {
				if dir == owner {
					return true
				}
			}
			violations = append(violations, rel+": "+sel.Sel.Name)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("only internal/proposals applies a proposal's change:\n%s", strings.Join(violations, "\n"))
	}
}
