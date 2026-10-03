package contractaudit

import (
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
