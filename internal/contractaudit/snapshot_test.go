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
// comment-only and implementation edits must not. Routes, their authority and
// error statuses are the generated HTTP contract's (internal/contract).
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
		{"changed public Go signature", inDir("adapters/gin"), goEdit(addParameter), "go_api adapters/gin added: func"},
		{"changed public JSON tag", inDir("billing"), goEdit(renameJSONTag), "go_api billing changed: type"},
		{"changed internal wire field type", under("internal/modules/"), goEdit(retypeJSONField), "wire_types internal/modules/"},
		{"changed custom JSON codec", under("internal/modules/"), goEdit(editJSONCodec), "wire_types internal/modules/"},
		{"changed error code", under("billing/error_codes.go"), goEdit(renameStringConst), "go_api billing added: const"},
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
		{"implementation edit", under("internal/http/routes/"), goEdit(editPlainFunc), ""},
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
