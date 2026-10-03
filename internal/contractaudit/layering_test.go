package contractaudit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// The root package is the client; it will construct the engine, so nothing
// under internal/ may import it. Shared nouns live in package billing (#1121).
func TestInternalDoesNotImportRoot(t *testing.T) {
	fsys := repositoryFS(t)
	err := fs.WalkDir(fsys, "internal", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return err
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, body, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			if path, _ := strconv.Unquote(spec.Path.Value); path == Module {
				t.Errorf("%s imports the root package; use %s/billing", name, Module)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Root's only aliases are the Config and Deps family (#1121): defined once in
// internal/config (auth hook types in internal/billingauth, the cache
// interface in internal/cache) and named in root's config.go. Every other
// root type is the Client's own.
func TestRootAliasesAreTheConfigFamily(t *testing.T) {
	fsys := repositoryFS(t)
	family := map[string]bool{Module + "/internal/config": true, Module + "/internal/billingauth": true, Module + "/internal/cache": true}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	aliases := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, body, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		imports := map[string]string{}
		for _, spec := range file.Imports {
			path, _ := strconv.Unquote(spec.Path.Value)
			local := path[strings.LastIndex(path, "/")+1:]
			if spec.Name != nil {
				local = spec.Name.Name
			}
			imports[local] = path
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts := spec.(*ast.TypeSpec)
				if !ts.Assign.IsValid() {
					continue
				}
				aliases++
				sel, ok := ts.Type.(*ast.SelectorExpr)
				pkg, _ := sel.X.(*ast.Ident)
				if name != "config.go" || !ok || pkg == nil || !family[imports[pkg.Name]] {
					t.Errorf("%s: root alias %s is outside the Config/Deps family (config.go, internal/config or internal/billingauth)", name, ts.Name.Name)
				}
			}
		}
	}
	if aliases == 0 {
		t.Fatal("root declares no Config/Deps aliases")
	}
}
