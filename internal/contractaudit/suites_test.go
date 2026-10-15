package contractaudit

import (
	"bufio"
	"bytes"
	"cmp"
	"go/build/constraint"
	"io/fs"
	"slices"
	"strings"
	"testing"
)

// A test built only under the suite tags runs in no `go test ./...`. CI builds
// those tags only for the end-to-end suites (scripts/e2e.sh) and the examples'
// own job, so such a test anywhere else never runs.
var (
	suiteTags = []string{"e2e", "integration"}
	suiteDirs = []string{"ci/", "server/ci/", "server/cmd/openrails/", "examples/"}
)

func TestEndToEndTestsLiveInTheSuites(t *testing.T) {
	fsys := repositoryFS(t)
	var stray []string
	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if base := entry.Name(); name != "." && (strings.HasPrefix(base, ".") || base == "node_modules" || base == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, "_test.go") || slices.ContainsFunc(suiteDirs, func(dir string) bool { return strings.HasPrefix(name, dir) }) {
			return nil
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		if tag := suiteTag(body); tag != "" {
			stray = append(stray, name+" ("+tag+")")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) > 0 {
		t.Fatalf("tests behind a suite build tag outside %s never run in CI; move them into an end-to-end suite:\n  %s", strings.Join(suiteDirs, ", "), strings.Join(stray, "\n  "))
	}
}

// suiteTag returns the suite tag a file's build constraint requires, if any.
func suiteTag(body []byte) string {
	lines := bufio.NewScanner(bytes.NewReader(body))
	for lines.Scan() {
		line := strings.TrimSpace(lines.Text())
		if strings.HasPrefix(line, "package ") {
			return ""
		}
		if !constraint.IsGoBuild(line) && !constraint.IsPlusBuild(line) {
			continue
		}
		expr, err := constraint.Parse(line)
		if err != nil {
			continue
		}
		if tag := requiredTag(expr, false); tag != "" {
			return tag
		}
	}
	return ""
}

func requiredTag(expr constraint.Expr, negated bool) string {
	switch x := expr.(type) {
	case *constraint.TagExpr:
		if !negated && slices.Contains(suiteTags, x.Tag) {
			return x.Tag
		}
	case *constraint.NotExpr:
		return requiredTag(x.X, !negated)
	case *constraint.AndExpr:
		return cmp.Or(requiredTag(x.X, negated), requiredTag(x.Y, negated))
	case *constraint.OrExpr:
		return cmp.Or(requiredTag(x.X, negated), requiredTag(x.Y, negated))
	}
	return ""
}
