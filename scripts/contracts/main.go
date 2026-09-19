// contracts verifies or rewrites the reviewed pre-v1 release contract and runs
// the release workflow matrix. Run it from the repository root.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/open-rails/openrails/internal/contractaudit"
)

const workflowReceipt = ".reports/v1-workflows.jsonl"

func main() {
	write := flag.Bool("write", false, "write the reviewed current contract")
	workflows := flag.Bool("workflows", false, "run compatibility/workflows.tsv against disposable PostgreSQL/Redis and require every matrix cell to pass")
	flag.Parse()
	root, err := os.OpenRoot(".")
	if err == nil {
		switch {
		case *workflows:
			err = runWorkflows(root)
		case *write:
			var snapshot []byte
			if snapshot, err = contractaudit.Capture(root.FS()); err == nil {
				err = root.WriteFile(contractaudit.SnapshotPath, snapshot, 0o644)
			}
		default:
			err = contractaudit.Verify(root.FS())
		}
		_ = root.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runWorkflows(root *os.Root) error {
	raw, err := root.ReadFile(contractaudit.WorkflowManifestPath)
	if err != nil {
		return err
	}
	rows, err := contractaudit.ParseWorkflowManifest(raw)
	if err != nil {
		return err
	}
	if err := root.MkdirAll(".reports", 0o755); err != nil {
		return err
	}
	receipt, err := root.Create(workflowReceipt)
	if err != nil {
		return err
	}
	defer receipt.Close()

	// The terminal's interrupt reaches the script directly so it can stop the
	// Compose services it started.
	var output bytes.Buffer
	// The reduced suite is the maintained set. Run every retained integration
	// and browser test, including money/concurrency regressions outside the
	// journey matrix; the same event stream must contain every named workflow.
	cmd := exec.CommandContext(context.Background(), "bash", "scripts/test_integration.sh", "-tags=integration,browser", "-json", "./...")
	cmd.Stdout = io.MultiWriter(receipt, &output)
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	if runErr != nil {
		printTestFailures(os.Stderr, output.Bytes())
	}
	report, err := contractaudit.QualifyWorkflows(rows, &output, runErr)
	fmt.Println(strings.Join(report, "\n"))
	fmt.Println("test events:", workflowReceipt)
	return err
}

// The suite also runs tests outside the workflow manifest. Keep their failure
// diagnostics visible even when every named workflow passed; the full JSON
// receipt remains available for output from interrupted processes.
func printTestFailures(w io.Writer, raw []byte) {
	type event struct {
		Action, Package, ImportPath, Test, Output string
	}
	var events []event
	failed := map[[2]string]bool{}
	for line := range bytes.SplitSeq(raw, []byte{'\n'}) {
		var e event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Package == "" {
			e.Package = e.ImportPath
		}
		events = append(events, e)
		if e.Action == "fail" || e.Action == "build-fail" {
			failed[[2]string{e.Package, e.Test}] = true
			fmt.Fprintf(w, "FAIL %s %s\n", e.Package, e.Test)
		}
	}
	for _, e := range events {
		if failed[[2]string{e.Package, e.Test}] && e.Output != "" {
			fmt.Fprint(w, e.Output)
		}
	}
}
