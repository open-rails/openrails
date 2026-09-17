package contractaudit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
)

const WorkflowManifestPath = "compatibility/workflows.tsv"

// The release matrix is closed: every scenario must be proven in every
// deployment, or the manifest must name the cell as a gap.
var (
	Scenarios   = []string{"purchase_recovery", "metered_billing", "merchant_isolation", "signed_delegation", "provider_uncertainty", "restart"}
	Deployments = []string{"embedded", "standalone", "saas"}

	testPath      = regexp.MustCompile(`^Test[A-Za-z0-9_]*(/[^/\s]+)*$`)
	pendingStatus = regexp.MustCompile(`^pending#[1-9][0-9]*$`)
)

// Workflow is one manifest row. Retained rows name a real integration test
// path (subtests identify the deployment); gap and pending rows record why the
// cell cannot qualify yet.
type Workflow struct {
	Scenario   string
	Deployment string
	Package    string
	Test       string
	Status     string
	Note       string
}

func (w Workflow) Retained() bool { return w.Status == "retained" }

func (w Workflow) TopLevelTest() string {
	name, _, _ := strings.Cut(w.Test, "/")
	return name
}

func (w Workflow) String() string {
	return fmt.Sprintf("%s/%s %s %s", w.Scenario, w.Deployment, w.Package, w.Test)
}

// ParseWorkflowManifest reads tab-separated rows:
// scenario, deployment, package, test path, status, note.
func ParseWorkflowManifest(raw []byte) ([]Workflow, error) {
	var rows []Workflow
	var problems []string
	seen := map[string]bool{}
	cells := map[string]bool{}
	for number, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 6 {
			problems = append(problems, fmt.Sprintf("line %d: want 6 tab-separated columns, got %d", number+1, len(fields)))
			continue
		}
		row := Workflow{fields[0], fields[1], fields[2], fields[3], fields[4], strings.TrimSpace(fields[5])}
		if problem := row.problem(); problem != "" {
			problems = append(problems, fmt.Sprintf("line %d: %s", number+1, problem))
			continue
		}
		if key := row.String(); seen[key] {
			problems = append(problems, fmt.Sprintf("line %d: duplicate row %s", number+1, key))
		} else {
			seen[key] = true
		}
		cells[row.Scenario+"/"+row.Deployment] = true
		rows = append(rows, row)
	}
	for _, scenario := range Scenarios {
		for _, deployment := range Deployments {
			if cell := scenario + "/" + deployment; !cells[cell] {
				problems = append(problems, "missing matrix cell "+cell)
			}
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid workflow manifest:\n  %s", strings.Join(problems, "\n  "))
	}
	return rows, nil
}

func (w Workflow) problem() string {
	if !slices.Contains(Scenarios, w.Scenario) {
		return "unknown scenario " + w.Scenario
	}
	if !slices.Contains(Deployments, w.Deployment) {
		return "unknown deployment " + w.Deployment
	}
	switch {
	case w.Status == "gap":
		if w.Package != "-" || w.Test != "-" {
			return "gap rows must not name a test"
		}
	case w.Status == "retained" || pendingStatus.MatchString(w.Status):
		if w.Package != "." && !strings.HasPrefix(w.Package, "./") {
			return "package must be a ./ path"
		}
		if !testPath.MatchString(w.Test) {
			return "invalid test path " + w.Test
		}
	default:
		return "status must be retained, gap or pending#<pull request>"
	}
	if !w.Retained() && w.Note == "" {
		return "gap and pending rows must state what is missing"
	}
	return ""
}

// WorkflowSelection returns the packages and anchored -run pattern for the
// retained rows.
func WorkflowSelection(rows []Workflow) ([]string, string, error) {
	packages, tests := map[string]bool{}, map[string]bool{}
	for _, row := range rows {
		if row.Retained() {
			packages[row.Package] = true
			tests[regexp.QuoteMeta(row.TopLevelTest())] = true
		}
	}
	if len(tests) == 0 {
		return nil, "", errors.New("workflow manifest has no retained workflows to run")
	}
	return keys(packages), "^(" + strings.Join(keys(tests), "|") + ")$", nil
}

type testEvent struct {
	Action  string
	Package string
	Test    string
}

// QualifyWorkflows evaluates `go test -json` output. Only explicit passes of
// each named test path qualify; skips, failures, build failures, missing
// events, a failed run and any gap or pending cell all fail closed.
func QualifyWorkflows(rows []Workflow, output io.Reader, runErr error) ([]string, error) {
	var events []testEvent
	reader := bufio.NewReader(output)
	for {
		line, err := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && trimmed[0] == '{' {
			var event testEvent
			if json.Unmarshal(trimmed, &event) == nil {
				events = append(events, event)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	var report, failures []string
	if runErr != nil {
		failures = append(failures, "test run failed: "+runErr.Error())
	}
	for _, row := range rows {
		if !row.Retained() {
			report = append(report, fmt.Sprintf("%s %s/%s: %s", strings.ToUpper(row.Status), row.Scenario, row.Deployment, row.Note))
			failures = append(failures, fmt.Sprintf("%s cell %s/%s", row.Status, row.Scenario, row.Deployment))
			continue
		}
		pkg := Module + strings.TrimPrefix(row.Package, ".")
		top := row.TopLevelTest()
		passed := false
		var problems []string
		for _, event := range events {
			switch {
			case event.Action == "build-fail":
				problems = append(problems, "build failed")
			case event.Package != pkg:
			case event.Test == "" && event.Action == "fail":
				problems = append(problems, "package failed")
			case event.Test == row.Test && event.Action == "pass":
				passed = true
			case (event.Test == top || strings.HasPrefix(event.Test, top+"/")) && (event.Action == "skip" || event.Action == "fail"):
				problems = append(problems, event.Action+" "+event.Test)
			}
		}
		if !passed {
			problems = append(problems, "no pass event")
		}
		slices.Sort(problems)
		problems = slices.Compact(problems)
		if len(problems) == 0 {
			report = append(report, "PASS "+row.String())
			continue
		}
		report = append(report, fmt.Sprintf("FAIL %s: %s", row, strings.Join(problems, ", ")))
		failures = append(failures, row.String())
	}
	if len(failures) > 0 {
		return report, fmt.Errorf("release workflow matrix is not qualified: %s", strings.Join(failures, "; "))
	}
	return report, nil
}
