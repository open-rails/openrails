package nmimock_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Only tests, test harnesses and the sandbox command may import the mock:
// production code paths must never reach a fake gateway.
func TestOnlyTestsAndSandboxImportTheMock(t *testing.T) {
	const self = "github.com/open-rails/openrails/nmimock"
	allowed := []string{"nmimock/", "cmd/openrails/sandbox_", "ci/", "sdk/billing-ui/e2e/"}
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || (strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, imp := range f.Imports {
			if p, _ := strconv.Unquote(imp.Path.Value); p != self && !strings.HasPrefix(p, self+"/") {
				continue
			}
			ok := false
			for _, prefix := range allowed {
				ok = ok || strings.HasPrefix(rel, prefix)
			}
			if !ok {
				t.Errorf("%s imports nmimock; only tests, test harnesses and sandbox commands may", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
