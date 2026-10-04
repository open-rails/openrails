// contracts verifies or rewrites the reviewed contracts: the Go API list
// (api/go.txt), the Go wire snapshot (compatibility/contract.json) and the
// files generated from the route catalog (api/openapi.json, the TypeScript
// wire types, the route and error-code tables in docs/api).
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path"

	"github.com/open-rails/openrails/internal/apisurface"
	"github.com/open-rails/openrails/internal/contract"
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
	surface, err := apisurface.Load(context.Background())
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
		if err := contractaudit.Verify(root.FS()); err != nil {
			return err
		}
		return contract.Verify(root.FS())
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
	if err := root.WriteFile(contractaudit.SnapshotPath, snapshot, 0o644); err != nil {
		return err
	}
	files, err := contract.Files(root.FS())
	if err != nil {
		return err
	}
	for name, body := range files {
		if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
			return err
		}
		if err := root.WriteFile(name, body, 0o644); err != nil {
			return err
		}
	}
	return nil
}
