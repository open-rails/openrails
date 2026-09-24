// contracts verifies or rewrites the reviewed pre-v1 release contract.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/open-rails/openrails/internal/contractaudit"
)

func main() {
	write := flag.Bool("write", false, "write the reviewed current contract")
	flag.Parse()

	root, err := os.OpenRoot(".")
	if err == nil {
		if *write {
			var snapshot []byte
			if snapshot, err = contractaudit.Capture(root.FS()); err == nil {
				err = root.WriteFile(contractaudit.SnapshotPath, snapshot, 0o644)
			}
		} else {
			err = contractaudit.Verify(root.FS())
		}
		_ = root.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
