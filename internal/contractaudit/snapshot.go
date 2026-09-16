// Package contractaudit records the source contracts reviewed for release.
package contractaudit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type Snapshot struct {
	API     map[string][]string `json:"go_api"`
	Sources map[string]string   `json:"boundary_sources_sha256"`
}

// Capture uses the Go AST rather than grep so receiver types, aliases, generic
// signatures and JSON tags stay part of the reviewed contract. Source hashes
// supplement the route/wire workflow fixtures for dynamic response maps and
// authorization code; they are review gates, not behavioral equivalence proof.
func Capture(root string) ([]byte, error) {
	out := Snapshot{API: map[string][]string{}, Sources: map[string]string{}}
	dirs := []string{".", "embed", "config", "permissions"}
	err := filepath.WalkDir(filepath.Join(root, "pkg"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			rel, e := filepath.Rel(root, path)
			if e != nil {
				return e
			}
			dirs = append(dirs, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			return nil, err
		}
		var declarations []string
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			fs := token.NewFileSet()
			parsed, err := parser.ParseFile(fs, file, nil, 0)
			if err != nil {
				return nil, err
			}
			imports := map[string]string{}
			for _, imp := range parsed.Imports {
				path, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return nil, err
				}
				name := filepath.Base(path)
				if imp.Name != nil {
					name = imp.Name.Name
				}
				imports[name] = path
			}
			for _, decl := range parsed.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if !ast.IsExported(d.Name.Name) {
						continue
					}
					if d.Recv != nil && !exportedReceiver(d.Recv.List[0].Type) {
						continue
					}
					d.Body = nil
					declarations = append(declarations, renderWithImports(fs, d, imports))
				case *ast.GenDecl:
					if d.Tok == token.IMPORT {
						continue
					}
					if d.Tok == token.CONST {
						exported := false
						for _, spec := range d.Specs {
							for _, name := range spec.(*ast.ValueSpec).Names {
								exported = exported || ast.IsExported(name.Name)
							}
						}
						if exported {
							declarations = append(declarations, renderWithImports(fs, d, imports))
						}
						continue
					}
					for _, spec := range d.Specs {
						switch s := spec.(type) {
						case *ast.TypeSpec:
							if !ast.IsExported(s.Name.Name) {
								continue
							}
							if shape, ok := s.Type.(*ast.StructType); ok {
								fields := shape.Fields.List[:0]
								for _, f := range shape.Fields.List {
									names := f.Names[:0]
									for _, name := range f.Names {
										if ast.IsExported(name.Name) {
											names = append(names, name)
										}
									}
									if len(f.Names) == 0 || len(names) > 0 {
										f.Names = names
										fields = append(fields, f)
									}
								}
								shape.Fields.List = fields
							}
							declarations = append(declarations, "type "+renderWithImports(fs, s, imports))
						case *ast.ValueSpec:
							for _, name := range s.Names {
								if ast.IsExported(name.Name) {
									declarations = append(declarations, d.Tok.String()+" "+renderWithImports(fs, s, imports))
									break
								}
							}
						}
					}
				}
			}
		}
		if len(declarations) > 0 {
			sort.Strings(declarations)
			out.API[filepath.ToSlash(dir)] = declarations
		}
	}
	for _, dir := range []string{"internal/http/routes", "internal/http/handlers", "pkg/billingauth", "internal/requestauth", "migrations/postgres", "testdata/wire"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry os.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".go" && ext != ".sql" && ext != ".json" {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(body)
			out.Sources[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func exportedReceiver(t ast.Expr) bool {
	switch t := t.(type) {
	case *ast.Ident:
		return ast.IsExported(t.Name)
	case *ast.StarExpr:
		return exportedReceiver(t.X)
	case *ast.IndexExpr:
		return exportedReceiver(t.X)
	case *ast.IndexListExpr:
		return exportedReceiver(t.X)
	default:
		return false
	}
}

func render(fs *token.FileSet, node ast.Node) string {
	var out bytes.Buffer
	if err := format.Node(&out, fs, node); err != nil {
		panic(fmt.Sprintf("format parsed contract: %v", err))
	}
	return out.String()
}

func renderWithImports(fs *token.FileSet, node ast.Node, imports map[string]string) string {
	referenced := map[string]bool{}
	ast.Inspect(node, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				if path, ok := imports[id.Name]; ok {
					referenced[id.Name+"="+path] = true
				}
			}
		}
		return true
	})
	names := make([]string, 0, len(referenced))
	for name := range referenced {
		names = append(names, name)
	}
	sort.Strings(names)
	return render(fs, node) + " [imports: " + strings.Join(names, ", ") + "]"
}
