package apisurface

import (
	"context"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

var tree = sync.OnceValues(func() (*Surface, error) {
	return Load(context.Background(), filepath.Join("..", ".."))
})

// lintTree returns the repository's findings and baseline.
func lintTree(t *testing.T) (*Surface, []string, []byte) {
	t.Helper()
	s, err := tree()
	if err != nil {
		t.Fatal(err)
	}
	findings, err := s.Lint()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(filepath.Join("..", "..", BaselineFile))
	if err != nil {
		t.Fatal(err)
	}
	return s, findings, baseline
}

func TestLintPassesOnTree(t *testing.T) {
	_, findings, baseline := lintTree(t)
	if _, err := Compare(findings, baseline); err != nil {
		t.Fatal(err)
	}
}

func TestLintFailsWithoutBaselineLine(t *testing.T) {
	_, findings, baseline := lintTree(t)
	line := findings[len(findings)/2]
	trimmed := strings.Replace(string(baseline), "\n"+line+"\n", "\n", 1)
	_, err := Compare(findings, []byte(trimmed))
	if err == nil || !strings.Contains(err.Error(), "new API findings") || !strings.Contains(err.Error(), line) {
		t.Fatalf("removing %q: %v", line, err)
	}
}

func TestLintFailsOnSeededFinding(t *testing.T) {
	s, _, baseline := lintTree(t)
	seeded := surfaceOf(append(slices.Clone(s.pkgs), seedPackage(t, `package seed

const Seeded = "seeded"
`)), s.pkgNames)
	findings, err := seeded.Lint()
	if err != nil {
		t.Fatal(err)
	}
	_, err = Compare(findings, baseline)
	if err == nil || !strings.Contains(err.Error(), "untyped-const seed.Seeded") {
		t.Fatalf("seeded untyped constant: %v", err)
	}
}

func TestLintFailsOnStaleLine(t *testing.T) {
	_, findings, baseline := lintTree(t)
	const stale = "verb openrails.Client.Gone"
	kept, err := Compare(findings, append(slices.Clone(baseline), stale+"\n"...))
	if err == nil || !strings.Contains(err.Error(), "match nothing") || !strings.Contains(err.Error(), stale) {
		t.Fatalf("stale %q: %v", stale, err)
	}
	// What -prune keeps: the baseline without the stale line.
	if slices.Contains(kept, stale) || !slices.Equal(kept, findings) {
		t.Fatalf("pruned baseline kept %d lines, want the %d findings", len(kept), len(findings))
	}
}

func TestLintFailsOnUnusedException(t *testing.T) {
	s, _, _ := lintTree(t)
	saved := exceptions
	t.Cleanup(func() { exceptions = saved })
	exceptions = append(slices.Clone(saved), exception{"verb", "openrails.Client.Gone", "test"})
	if _, err := s.Lint(); err == nil || !strings.Contains(err.Error(), "openrails.Client.Gone") {
		t.Fatalf("unused exception: %v", err)
	}
}

// seedPackage type-checks src as an extra covered package.
func seedPackage(t *testing.T, src string) Package {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "seed.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check(Module+"/seed", fset, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Package{Name: "seed", Types: pkg}
}
