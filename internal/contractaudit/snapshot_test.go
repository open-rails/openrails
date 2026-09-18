package contractaudit

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestReviewedReleaseContract(t *testing.T) {
	if err := Verify(repositoryFS(t)); err != nil {
		t.Fatal(err)
	}
}

// The gate is proven against a copy-on-write view of the real repository: each
// covered category is mutated in an actual source file and must drift, while
// comment-only and non-boundary implementation edits must not.
func TestContractGateDetectsCoveredMutations(t *testing.T) {
	base := repositoryFS(t)
	c := newCapturer()
	snapshot, err := c.capture(base)
	if err != nil {
		t.Fatal(err)
	}
	fresh := overlayFS{base, map[string][]byte{SnapshotPath: snapshot}}
	if err := c.verify(fresh); err != nil {
		t.Fatalf("unmutated tree must match its own snapshot: %v", err)
	}
	gate := func(t *testing.T, m mutation) error {
		t.Helper()
		name, body := m.apply(t, base)
		return c.verify(overlayFS{base, map[string][]byte{SnapshotPath: snapshot, name: body}})
	}

	for _, m := range []mutation{
		{"removed public Go method", inDir("."), goEdit(removeExportedMethod), "go_api . removed: func"},
		{"changed public Go signature", inDir("embed"), goEdit(addParameter), "go_api embed added: func"},
		{"changed public JSON tag", inDir("."), goEdit(renameJSONTag), "go_api . changed: type"},
		{"changed internal wire field type", under("internal/modules/"), goEdit(retypeJSONField), "wire_types internal/modules/"},
		{"changed custom JSON codec", under("internal/modules/"), goEdit(editJSONCodec), "wire_types internal/modules/"},
		{"changed HTTP status mapping", under("pkg/api/"), goEdit(changeStatusInUnexportedFunc), "boundary source changed: pkg/api/"},
		{"changed error code", under("pkg/api/"), goEdit(renameStringConst), "go_api pkg/api added: const"},
		{"changed route path", under("internal/http/routes/"), goEdit(renameRoutePath), "boundary source changed: internal/http/routes/"},
		{"changed route authority", under("internal/http/routes/"), goEdit(swapPermission), "boundary source changed: internal/http/routes/"},
		{"changed role permission mapping", under("permissions/"), goEdit(editFuncString), "boundary source changed: permissions/"},
		{"changed authority decision outside HTTP", outsideBoundaryPrefixes, goEdit(editAuthorityFile), "boundary source changed: "},
		{"changed schema invariant", under("migrations/postgres/"), textEdit(".sql", " NOT NULL", ""), "boundary source changed: migrations/postgres/"},
		{"changed canonical wire fixture", under("testdata/wire/"), textEdit(".json", `"`, `"renamed_`), "boundary source changed: testdata/wire/"},
	} {
		t.Run(m.name, func(t *testing.T) {
			err := gate(t, m)
			if !errors.Is(err, ErrContractDrift) || !strings.Contains(err.Error(), m.want) {
				t.Fatalf("mutation was not reported as %q drift: %v", m.want, err)
			}
		})
	}
	for _, m := range []mutation{
		{"comment-only route edit", under("internal/http/routes/"), goEdit(addComment), ""},
		{"non-boundary implementation edit", under("internal/modules/"), goEdit(editPlainFunc), ""},
	} {
		t.Run(m.name, func(t *testing.T) {
			if err := gate(t, m); err != nil {
				t.Fatalf("edit outside the reviewed contract drifted: %v", err)
			}
		})
	}
}

func repositoryFS(t *testing.T) fs.FS {
	t.Helper()
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root.FS()
}

type overlayFS struct {
	fs.FS
	files map[string][]byte
}

func (o overlayFS) Open(name string) (fs.File, error) {
	if body, ok := o.files[name]; ok {
		return fstest.MapFS{name: {Data: body, Mode: 0o644}}.Open(name)
	}
	return o.FS.Open(name)
}

type mutation struct {
	name  string
	scope func(name string) bool
	edit  func(name string, body []byte) ([]byte, bool)
	want  string
}

// apply edits the first matching real file in walk order and fails when the
// category no longer has a target, so coverage cannot silently vanish.
func (m mutation) apply(t *testing.T, fsys fs.FS) (string, []byte) {
	t.Helper()
	var name string
	var mutated []byte
	err := fs.WalkDir(fsys, ".", func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return walkDir(fsys, current, entry.Name())
		}
		if !covered(current) || !m.scope(current) {
			return nil
		}
		body, err := fs.ReadFile(fsys, current)
		if err != nil {
			return err
		}
		if edited, ok := m.edit(current, body); ok {
			if bytes.Equal(edited, body) {
				t.Fatalf("%s: mutation of %s changed nothing", m.name, current)
			}
			name, mutated = current, edited
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if name == "" {
		t.Fatalf("%s: no target file found", m.name)
	}
	return name, mutated
}

func inDir(dir string) func(string) bool {
	return func(name string) bool { return path.Dir(name) == dir }
}

func under(prefix string) func(string) bool {
	return func(name string) bool { return strings.HasPrefix(name, prefix) }
}

func outsideBoundaryPrefixes(name string) bool {
	for _, prefix := range boundaryPrefixes {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return !boundaryFiles[name]
}

func textEdit(ext, old, replacement string) func(string, []byte) ([]byte, bool) {
	return func(name string, body []byte) ([]byte, bool) {
		if path.Ext(name) != ext || !bytes.Contains(body, []byte(old)) {
			return nil, false
		}
		return bytes.Replace(body, []byte(old), []byte(replacement), 1), true
	}
}

type goEditor func(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool)

func goEdit(editor goEditor) func(string, []byte) ([]byte, bool) {
	return func(name string, body []byte) ([]byte, bool) {
		if path.Ext(name) != ".go" {
			return nil, false
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, body, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, false
		}
		return editor(file, func(pos token.Pos) int { return fset.Position(pos).Offset }, body)
	}
}

func splice(body []byte, start, end int, replacement string) []byte {
	out := append([]byte{}, body[:start]...)
	out = append(out, replacement...)
	return append(out, body[end:]...)
}

func removeExportedMethod(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Name.IsExported() && !wireCodecMethods[fn.Name.Name] && exportedReceiver(fn.Recv.List[0].Type) {
			return splice(body, offset(fn.Pos()), offset(fn.End()), ""), true
		}
	}
	return nil, false
}

func addParameter(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.IsExported() && (fn.Recv == nil || exportedReceiver(fn.Recv.List[0].Type)) {
			param := ", contractMutation int"
			if len(fn.Type.Params.List) == 0 {
				param = "contractMutation int"
			}
			closing := offset(fn.Type.Params.Closing)
			return splice(body, closing, closing, param), true
		}
	}
	return nil, false
}

func renameJSONTag(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	var out []byte
	ast.Inspect(file, func(node ast.Node) bool {
		if field, ok := node.(*ast.Field); ok && out == nil && field.Tag != nil {
			if at := strings.Index(field.Tag.Value, `json:"`); at >= 0 {
				start := offset(field.Tag.Pos()) + at + len(`json:"`)
				out = splice(body, start, start, "renamed_")
			}
		}
		return out == nil
	})
	return out, out != nil
}

func retypeJSONField(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	var out []byte
	ast.Inspect(file, func(node ast.Node) bool {
		if field, ok := node.(*ast.Field); ok && out == nil && len(field.Names) > 0 && field.Names[0].IsExported() && field.Tag != nil && strings.Contains(field.Tag.Value, `json:"`) {
			out = splice(body, offset(field.Type.Pos()), offset(field.Type.End()), "any")
		}
		return out == nil
	})
	return out, out != nil
}

func editJSONCodec(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && wireCodecMethods[fn.Name.Name] && fn.Body != nil {
			at := offset(fn.Body.Lbrace) + 1
			return splice(body, at, at, "\n_ = 0\n"), true
		}
	}
	return nil, false
}

func changeStatusInUnexportedFunc(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.IsExported() || fn.Body == nil {
			continue
		}
		var out []byte
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && out == nil && isIdent(selector.X, "http") && strings.HasPrefix(selector.Sel.Name, "Status") {
				replacement := "StatusTeapot"
				if selector.Sel.Name == replacement {
					replacement = "StatusGone"
				}
				out = splice(body, offset(selector.Sel.Pos()), offset(selector.Sel.End()), replacement)
			}
			return out == nil
		})
		if out != nil {
			return out, true
		}
	}
	return nil, false
}

func renameStringConst(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				if len(value.Values) > 0 && value.Names[0].IsExported() {
					if literal, ok := value.Values[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						at := offset(literal.Pos()) + 1
						return splice(body, at, at, "renamed_"), true
					}
				}
			}
		}
	}
	return nil, false
}

func renameRoutePath(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	var out []byte
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && out == nil && len(call.Args) >= 3 {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Handle" {
				if literal, ok := call.Args[1].(*ast.BasicLit); ok && literal.Kind == token.STRING {
					at := offset(literal.End()) - 1
					out = splice(body, at, at, "/renamed")
				}
			}
		}
		return out == nil
	})
	return out, out != nil
}

func swapPermission(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	var out []byte
	ast.Inspect(file, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && out == nil && isIdent(selector.X, "permissions") && selector.Sel.IsExported() {
			replacement := "MerchantAll"
			if selector.Sel.Name == replacement {
				replacement = "CustomerAll"
			}
			out = splice(body, offset(selector.Sel.Pos()), offset(selector.Sel.End()), replacement)
		}
		return out == nil
	})
	return out, out != nil
}

func editFuncString(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var out []byte
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if literal, ok := node.(*ast.BasicLit); ok && out == nil && literal.Kind == token.STRING && len(literal.Value) > 2 {
				at := offset(literal.Pos()) + 1
				out = splice(body, at, at, "renamed_")
			}
			return out == nil
		})
		if out != nil {
			return out, true
		}
	}
	return nil, false
}

func editAuthorityFile(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	authority := false
	for _, spec := range file.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		if importPath == "testing" {
			return nil, false
		}
		authority = authority || authorityImports[importPath]
	}
	if !authority {
		return nil, false
	}
	return editPlainFunc(file, offset, body)
}

func editPlainFunc(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil && !wireCodecMethods[fn.Name.Name] {
			at := offset(fn.Body.Lbrace) + 1
			return splice(body, at, at, "\n_ = 0\n"), true
		}
	}
	return nil, false
}

func addComment(file *ast.File, offset func(token.Pos) int, body []byte) ([]byte, bool) {
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			at := offset(fn.Pos())
			return splice(body, at, at, "// Reviewed wording only.\n"), true
		}
	}
	return nil, false
}

func isIdent(expr ast.Expr, name string) bool {
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == name
}
