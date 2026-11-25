package riverjobs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// River refuses to insert a unique job whose ByState leaves out a state it
// requires: the insert fails, and so does whatever transaction made it.
var requiredUniqueStates = []string{"JobStateAvailable", "JobStatePending", "JobStateRunning", "JobStateScheduled"}

// Every river.UniqueOpts in the repository that names its states names
// River's required ones, inline or through a package-level variable.
func TestUniqueStatesIncludeRiversRequired(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root: %v", err)
	}
	checked := 0
	for dir, files := range goSources(t, root) {
		for _, problem := range uniqueStateProblems(t, files, &checked) {
			t.Errorf("%s: %s", dir, problem)
		}
	}
	if checked < 10 {
		t.Fatalf("found %d unique state lists; the scan missed them", checked)
	}
}

// goSources is each directory's non-test Go files under root, parsed.
func goSources(t *testing.T, root string) map[string][]*ast.File {
	t.Helper()
	out := map[string][]*ast.File{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "testdata", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		out[filepath.Dir(path)] = append(out[filepath.Dir(path)], f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// uniqueStateProblems checks one package's UniqueOpts literals.
func uniqueStateProblems(t *testing.T, files []*ast.File, checked *int) []string {
	t.Helper()
	vars := map[string]ast.Expr{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						vars[name.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	var problems []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isSelector(lit.Type, "river", "UniqueOpts") {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok || !isIdent(kv.Key, "ByState") {
					continue
				}
				states, ok := stateNames(kv.Value, vars)
				if !ok {
					problems = append(problems, "a UniqueOpts ByState the guard cannot read: name the states inline or in a package-level variable")
					continue
				}
				*checked++
				for _, want := range requiredUniqueStates {
					if !slices.Contains(states, want) {
						problems = append(problems, "a UniqueOpts ByState leaves out rivertype."+want+", which River requires")
					}
				}
			}
			return true
		})
	}
	return problems
}

// stateNames is the rivertype.JobState names a ByState value lists.
func stateNames(expr ast.Expr, vars map[string]ast.Expr) ([]string, bool) {
	if id, ok := expr.(*ast.Ident); ok {
		v, found := vars[id.Name]
		if !found {
			return nil, false
		}
		expr = v
	}
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return nil, false
	}
	var out []string
	for _, elt := range lit.Elts {
		sel, ok := elt.(*ast.SelectorExpr)
		if !ok || !isIdent(sel.X, "rivertype") {
			return nil, false
		}
		out = append(out, sel.Sel.Name)
	}
	return out, true
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, pkg) && sel.Sel.Name == name
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}
