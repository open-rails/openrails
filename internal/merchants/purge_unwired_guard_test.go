package merchants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// or#858's operative constraint, made structural.
//
// A merchant purge is ONE-WAY. There is no per-merchant snapshot to restore
// from (or#859 phase 2), so the only recovery is whole-cluster Postgres PITR.
// While that is true, Service.Delete must not be reachable from an operator
// surface — no HTTP route, no CLI command, no River job. The tracker says so;
// this test is what makes saying so cost something.
//
// The guard is exact rather than textual-ish: Delete cannot be called without a
// merchants.DeleteOptions value, and any caller outside this package must NAME
// that type to build one. A composite literal or a declaration of
// merchants.DeleteOptions anywhere else in the tree fails here.
//
// To lift this: land a real per-merchant snapshot + restore, gate Delete on it,
// then delete this test in the same change — not before.
func TestMerchantPurgeStaysUnwired(t *testing.T) {
	root := repoRoot(t)
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "merchants":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel == nil || sel.Sel.Name != "DeleteOptions" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "merchants" {
				return true
			}
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel+":"+fset.Position(sel.Pos()).String())
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	if len(offenders) > 0 {
		t.Fatalf("merchants.DeleteOptions is constructed outside internal/merchants, so the one-way merchant purge has an operator surface:\n  %s\n\n"+
			"A purge destroys 13 tables of customer-bearing state with no restore point other than whole-cluster Postgres PITR. "+
			"The purge inventory is NOT a backup. Land or#859 phase 2 (openrails merchant snapshot/restore), gate Delete on a real snapshot, "+
			"and remove this test in that same change.", strings.Join(offenders, "\n  "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above cwd")
		}
		dir = parent
	}
}
