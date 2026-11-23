package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/configdocument"
)

func fixtureIndex(t *testing.T) *Index {
	t.Helper()
	goTxt, err := os.ReadFile("testdata/go.txt")
	if err != nil {
		t.Fatal(err)
	}
	return &Index{
		Routes:      map[string]bool{"GET /v1/me/orders": true},
		Paths:       [][]string{{"v1", "me", "orders"}},
		Codes:       map[string]int{"merchant_unresolved": 403},
		Permissions: map[string]bool{"merchant:catalog:read": true},
		Events:      map[string]bool{"order.completed": true},
		JSONFields:  map[string]bool{},
		Schema:      map[string]map[string]bool{},
		Go:          parseGoTxt(string(goTxt)),
		ParseYAML: func(raw []byte) error {
			_, err := configdocument.YAMLToJSON(raw, 1<<20)
			return err
		},
	}
}

// fixture copies testdata/repo to a temporary tree, applying edits
// (path to text appended).
func fixture(t *testing.T, edits map[string]string) Tree {
	t.Helper()
	dir := t.TempDir()
	err := filepath.WalkDir("testdata/repo", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel("testdata/repo", p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		b = append(b, edits[filepath.ToSlash(rel)]...)
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := listFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	return Tree{Root: dir, Files: files}
}

func run1(t *testing.T, tree Tree, idx *Index, write bool) []Finding {
	t.Helper()
	rep, err := Check(tree, idx, write)
	if err != nil {
		t.Fatal(err)
	}
	return rep.Findings
}

func wantOnly(t *testing.T, got []Finding, class, detail string) {
	t.Helper()
	if len(got) != 1 || got[0].Class != class || !strings.Contains(got[0].Detail, detail) {
		t.Fatalf("want one %s finding with %q, got %+v", class, detail, got)
	}
}

func TestCleanFixturePasses(t *testing.T) {
	if got := run1(t, fixture(t, nil), fixtureIndex(t), false); len(got) != 0 {
		t.Fatalf("clean fixture: %+v", got)
	}
}

func TestBrokenLinkFails(t *testing.T) {
	got := run1(t, fixture(t, map[string]string{"README.md": "\nSee [the missing page](docs/missing.md).\n"}), fixtureIndex(t), false)
	wantOnly(t, got, cLink, "no such file")
}

func TestBrokenAnchorFails(t *testing.T) {
	got := run1(t, fixture(t, map[string]string{"README.md": "\nSee [nothing](docs/guide.md#no-such-heading).\n"}), fixtureIndex(t), false)
	wantOnly(t, got, cLink, "no heading")
}

func TestStaleSnippetFailsAndWriteFixesIt(t *testing.T) {
	tree := fixture(t, nil)
	guide := filepath.Join(tree.Root, "docs", "guide.md")
	b, _ := os.ReadFile(guide)
	if err := os.WriteFile(guide, []byte(strings.Replace(string(b), `"bye"`, `"farewell"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := fixtureIndex(t)
	wantOnly(t, run1(t, tree, idx, false), cStale, "guide:demo-hello")
	if got := run1(t, tree, idx, true); len(got) != 0 {
		t.Fatalf("-write: %+v", got)
	}
	if got := run1(t, tree, idx, false); len(got) != 0 {
		t.Fatalf("after -write: %+v", got)
	}
}

func TestDeadReferencesFail(t *testing.T) {
	got := run1(t, fixture(t, map[string]string{"docs/client.md": "\n`openrails.Nope` and `GET /v1/me/nope`.\n"}), fixtureIndex(t), false)
	if len(got) != 2 || got[0].Class != cIdent || got[1].Class != cRoute {
		t.Fatalf("want an identifier and a route finding, got %+v", got)
	}
}

func TestMissingMethodFailsCoverage(t *testing.T) {
	idx := fixtureIndex(t)
	idx.Coverage = &Coverage{Methods: map[string][]string{"CreateOrder": {"Client"}, "ListProducts": {"Client"}, "RefundPayment": {"Client"}}}
	wantOnly(t, run1(t, fixture(t, nil), idx, false), cCoverage, "RefundPayment")
}

func TestArchitectureDocsAreOnlyLinkChecked(t *testing.T) {
	tree := fixture(t, nil)
	page := filepath.Join(tree.Root, "docs", "architecture", "ledger.md")
	if err := os.MkdirAll(filepath.Dir(page), 0o755); err != nil {
		t.Fatal(err)
	}
	internal := "# Ledger\n\n`NoSuchFunc` posts to `billing.no_such_table` on `POST /v1/no/such/route`.\n\n```go\nx := 1\n```\n"
	if err := os.WriteFile(page, []byte(internal), 0o644); err != nil {
		t.Fatal(err)
	}
	tree.Files = append(tree.Files, "docs/architecture/ledger.md")
	if got := run1(t, tree, fixtureIndex(t), false); len(got) != 0 {
		t.Fatalf("architecture page drew reference or fence findings: %+v", got)
	}
	if err := os.WriteFile(page, []byte(internal+"\nSee [gone](missing.md).\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantOnly(t, run1(t, tree, fixtureIndex(t), false), cLink, "no such file")
}
