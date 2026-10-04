// contracts verifies or rewrites the reviewed contracts: the pre-v1 release
// contract (compatibility/contract.json) and the Go API list (api/go.txt).
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"

	"github.com/open-rails/openrails/internal/apisurface"
	"github.com/open-rails/openrails/internal/contractaudit"
)

func main() {
	write := flag.Bool("write", false, "write the reviewed current contracts")
	flag.Parse()
	if err := run(*write); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(write bool) error {
	root, err := os.OpenRoot(".")
	if err != nil {
		return err
	}
	defer root.Close()
	surface, err := apisurface.Load()
	if err != nil {
		return err
	}
	if !write {
		listed, err := root.ReadFile(apisurface.File)
		if err != nil {
			return err
		}
		if !bytes.Equal(listed, surface.Text()) {
			return apisurface.ErrStale
		}
		return contractaudit.Verify(root.FS())
	}
	if err := root.MkdirAll("api", 0o755); err != nil {
		return err
	}
	if err := root.WriteFile(apisurface.File, surface.Text(), 0o644); err != nil {
		return err
	}
	snapshot, err := contractaudit.Capture(root.FS())
	if err != nil {
		return err
	}
	return root.WriteFile(contractaudit.SnapshotPath, snapshot, 0o644)
}
