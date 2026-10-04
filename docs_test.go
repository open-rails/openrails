package openrails

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// trackerReference is a private issue number; it means nothing to a host.
var trackerReference = regexp.MustCompile(`#\d+`)

// TestExportedIdentifiersAreDocumented keeps this package's godoc complete:
// every exported identifier has its own doc comment, a function's or type's
// comment starts with its name, and none cites a tracker issue.
func TestExportedIdentifiersAreDocumented(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, entry := range entries {
		if name := entry.Name(); strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			file, err := parser.ParseFile(fset, name, nil, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, file)
		}
	}
	check := func(pos token.Pos, name string, doc *ast.CommentGroup, named bool) {
		at := fset.Position(pos)
		switch text := doc.Text(); {
		case strings.TrimSpace(text) == "":
			t.Errorf("%s: %s has no doc comment", at, name)
		case named && !strings.HasPrefix(text, name+" ") && !strings.HasPrefix(text, "A "+name+" ") && !strings.HasPrefix(text, "An "+name+" "):
			t.Errorf("%s: the doc comment of %s does not start with its name", at, name)
		case trackerReference.MatchString(text):
			t.Errorf("%s: the doc comment of %s cites a tracker issue", at, name)
		}
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				name := d.Name.Name
				if d.Recv != nil {
					recv := d.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					if id, ok := recv.(*ast.Ident); !ok || !id.IsExported() {
						continue
					}
				}
				if d.Name.IsExported() {
					check(d.Pos(), name, d.Doc, true)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !s.Name.IsExported() {
							continue
						}
						doc := s.Doc
						if doc == nil && len(d.Specs) == 1 {
							doc = d.Doc
						}
						check(s.Pos(), s.Name.Name, doc, true)
						if st, ok := s.Type.(*ast.StructType); ok {
							for _, field := range st.Fields.List {
								if text := field.Doc.Text() + field.Comment.Text(); trackerReference.MatchString(text) {
									t.Errorf("%s: a field comment of %s cites a tracker issue", fset.Position(field.Pos()), s.Name.Name)
								}
							}
						}
					case *ast.ValueSpec:
						doc := s.Doc
						if doc == nil {
							doc = s.Comment
						}
						if doc == nil && len(d.Specs) == 1 {
							doc = d.Doc
						}
						for _, name := range s.Names {
							if name.IsExported() {
								check(name.Pos(), name.Name, doc, false)
							}
						}
					}
				}
			}
		}
	}
}
