package contractaudit

import (
	"errors"
	"go/ast"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"runtime"
	"strings"
	"testing"
)

// Retained rows must name top-level tests that exist in integration-tagged
// files, so renaming or deleting a release workflow fails the unit gate
// before anyone runs the manifest.
func TestWorkflowManifestNamesRetainedIntegrationTests(t *testing.T) {
	fsys := repositoryFS(t)
	raw, err := fs.ReadFile(fsys, WorkflowManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ParseWorkflowManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Retained() && !integrationTestExists(t, fsys, strings.TrimPrefix(strings.TrimPrefix(row.Package, "."), "/"), row.TopLevelTest()) {
			t.Errorf("%s: no integration-tagged func %s(t *testing.T) in %s", row, row.TopLevelTest(), row.Package)
		}
	}
}

func TestWorkflowManifestRejectsIncompleteMatrices(t *testing.T) {
	raw, err := fs.ReadFile(repositoryFS(t), WorkflowManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(raw)
	var firstRow string
	for _, line := range strings.Split(manifest, "\n") {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			firstRow = line
			break
		}
	}
	withoutCell := func(cell string) string {
		var kept []string
		for _, line := range strings.Split(manifest, "\n") {
			if !strings.HasPrefix(line, strings.Replace(cell, "/", "\t", 1)+"\t") {
				kept = append(kept, line)
			}
		}
		return strings.Join(kept, "\n")
	}
	for name, candidate := range map[string]string{
		"empty":              "",
		"comments only":      "# nothing\n",
		"missing cell":       withoutCell("restart/saas"),
		"duplicate row":      manifest + firstRow + "\n",
		"gap names a test":   manifest + "restart\tsaas\t./embed\tTestX\tgap\tnot a gap\n",
		"gap without note":   manifest + "restart\tembedded\t-\t-\tgap\t\n",
		"unknown status":     manifest + "restart\tsaas\t./embed\tTestX\tskipped\tlater\n",
		"unknown scenario":   manifest + "catalog\tsaas\t./embed\tTestX\tretained\t\n",
		"wrong column":       manifest + "restart\tsaas\t./embed\tTestX\tretained\n",
		"not a test path":    manifest + "restart\tsaas\t./embed\tExampleX\tretained\t\n",
		"unnumbered pending": manifest + "restart\tsaas\t./embed\tTestX\tpending\tlater\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseWorkflowManifest([]byte(candidate)); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestWorkflowQualificationFailsClosed(t *testing.T) {
	embedded := Workflow{Scenario: "restart", Deployment: "embedded", Package: "./embed", Test: "TestWorkflow/embedded", Status: "retained"}
	standalone := Workflow{Scenario: "restart", Deployment: "standalone", Package: "./internal/integrationharness", Test: "TestOther", Status: "retained"}
	rows := []Workflow{embedded, standalone}
	const (
		embedPkg   = `"Package":"` + Module + `/embed"`
		harnessPkg = `"Package":"` + Module + `/internal/integrationharness"`
	)
	pass := strings.Join([]string{
		`{"Action":"run",` + embedPkg + `,"Test":"TestWorkflow"}`,
		`{"Action":"pass",` + embedPkg + `,"Test":"TestWorkflow/embedded"}`,
		`{"Action":"pass",` + embedPkg + `,"Test":"TestWorkflow/remote"}`,
		`{"Action":"pass",` + embedPkg + `,"Test":"TestWorkflow"}`,
		`{"Action":"pass",` + embedPkg + `}`,
		`{"Action":"pass",` + harnessPkg + `,"Test":"TestOther"}`,
		`{"Action":"pass",` + harnessPkg + `}`,
	}, "\n") + "\n"
	if report, err := QualifyWorkflows(rows, strings.NewReader(pass), nil); err != nil {
		t.Fatalf("complete passing run rejected: %v\n%s", err, strings.Join(report, "\n"))
	}
	for name, tc := range map[string]struct {
		rows   []Workflow
		output string
		runErr error
	}{
		"empty output":                 {rows, "", nil},
		"no tests to run":              {rows, "ok  \t" + Module + "/embed\t0.01s [no tests to run]\n", nil},
		"required subtest skipped":     {rows, strings.Replace(pass, `"pass",`+embedPkg+`,"Test":"TestWorkflow/embedded"`, `"skip",`+embedPkg+`,"Test":"TestWorkflow/embedded"`, 1), nil},
		"sibling subtest skipped":      {rows, pass + `{"Action":"skip",` + embedPkg + `,"Test":"TestWorkflow/remote"}` + "\n", nil},
		"top-level test skipped":       {rows, strings.Replace(pass, `"pass",`+harnessPkg+`,"Test":"TestOther"`, `"skip",`+harnessPkg+`,"Test":"TestOther"`, 1), nil},
		"required subtest missing":     {rows, strings.Replace(pass, `"Test":"TestWorkflow/embedded"`, `"Test":"TestWorkflow/renamed"`, 1), nil},
		"pass in another package":      {rows, strings.ReplaceAll(pass, harnessPkg, `"Package":"`+Module+`/pkg/service"`), nil},
		"nested failure":               {rows, pass + `{"Action":"fail",` + embedPkg + `,"Test":"TestWorkflow/embedded/step"}` + "\n", nil},
		"package failure":              {rows, pass + `{"Action":"fail",` + harnessPkg + `}` + "\n", nil},
		"build failure":                {rows, pass + `{"ImportPath":"` + Module + `/embed [test]","Action":"build-fail"}` + "\n", nil},
		"failed test process":          {rows, pass, errors.New("exit status 1")},
		"gap cell":                     {append(rows, Workflow{Scenario: "restart", Deployment: "saas", Package: "-", Test: "-", Status: "gap", Note: "missing"}), pass, nil},
		"pending cell":                 {append(rows, Workflow{Scenario: "restart", Deployment: "saas", Package: "./embed", Test: "TestPending", Status: "pending#1", Note: "open"}), pass, nil},
		"plain text claiming a pass":   {rows, "--- PASS: TestWorkflow/embedded\n--- PASS: TestOther\nPASS\n", nil},
		"malformed json claiming pass": {rows, `{"Action":"pass",` + embedPkg + `,"Test":"TestWorkflow/embedded"` + "\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := QualifyWorkflows(tc.rows, strings.NewReader(tc.output), tc.runErr); err == nil {
				t.Fatal("unqualified run accepted")
			}
		})
	}
	if _, _, err := WorkflowSelection([]Workflow{{Status: "gap"}}); err == nil {
		t.Fatal("a manifest without retained workflows selected an empty suite")
	}
	packages, pattern, err := WorkflowSelection(rows)
	if err != nil || strings.Join(packages, " ") != "./embed ./internal/integrationharness" || pattern != "^(TestOther|TestWorkflow)$" {
		t.Fatalf("selection = %v %q %v", packages, pattern, err)
	}
}

func integrationTestExists(t *testing.T, fsys fs.FS, dir, name string) bool {
	t.Helper()
	files, err := fs.Glob(fsys, path.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, body, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		if !requiresIntegrationTag(parsed) {
			continue
		}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name && len(fn.Type.Params.List) == 1 {
				if star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr); ok {
					if selector, ok := star.X.(*ast.SelectorExpr); ok && isIdent(selector.X, "testing") && selector.Sel.Name == "T" {
						return true
					}
				}
			}
		}
	}
	return false
}

// requiresIntegrationTag reports whether the file builds with the runner's
// integration tag and not without it. Browser is an orthogonal opt-in used by
// the same E2E runner; evaluate it as enabled while still requiring integration
// to be the tag that changes the file's build decision.
func requiresIntegrationTag(file *ast.File) bool {
	for _, group := range file.Comments {
		if group.Pos() > file.Package {
			break
		}
		for _, comment := range group.List {
			if !constraint.IsGoBuild(comment.Text) {
				continue
			}
			expr, err := constraint.Parse(comment.Text)
			if err != nil {
				return false
			}
			with := func(integration bool) bool {
				return expr.Eval(func(tag string) bool {
					return (integration && tag == "integration") || tag == "browser" || tag == "provider_qualification" || tag == runtime.GOOS || tag == runtime.GOARCH || tag == "unix"
				})
			}
			return with(true) && !with(false)
		}
	}
	return false
}
