// contracts verifies or rewrites the reviewed pre-v1 release contract and runs
// the release workflow matrix. Run it from the repository root.
package main

import (
	"bytes"
	"context"
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
	report, err := contractaudit.QualifyWorkflows(rows, &output, runErr)
	fmt.Println(strings.Join(report, "\n"))
	fmt.Println("test events:", workflowReceipt)
	return err
}
