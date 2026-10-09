// contracts verifies or rewrites the reviewed contracts: the Go API list
// (api/go.txt), the schema list (api/schema.txt), the Go wire snapshot
// (compatibility/contract.json) and the files generated from the route
// catalog (api/openapi.json, the TypeScript wire types, the route and
// error-code tables in docs/api).
//
// The schema list is read from a real PostgreSQL: -dsn, or OPENRAILS_E2E_DSN,
// names a disposable server. Without one the list is only checked against the
// migration files, and -write refuses to leave it behind them.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/open-rails/openrails/internal/apisurface"
	"github.com/open-rails/openrails/internal/contract"
	"github.com/open-rails/openrails/internal/contractaudit"
	"github.com/open-rails/openrails/internal/schemasnapshot"
)

func main() {
	write := flag.Bool("write", false, "write the reviewed current contracts")
	dsn := flag.String("dsn", os.Getenv(schemasnapshot.DSNEnv), "disposable PostgreSQL server the schema list is read from")
	flag.Parse()
	if err := run(*write, strings.TrimSpace(*dsn)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(write bool, dsn string) error {
	root, err := os.OpenRoot(".")
	if err != nil {
		return err
	}
	defer root.Close()
	ctx := context.Background()
	surface, err := apisurface.Load(ctx, ".")
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
		if err := contract.Verify(root.FS()); err != nil {
			return err
		}
		return schema(ctx, root, dsn, false)
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
	return schema(ctx, root, dsn, true)
}

// schema checks or rewrites api/schema.txt. With no server it can only tell
// whether the list was taken from the migration files as they are now.
func schema(ctx context.Context, root *os.Root, dsn string, write bool) error {
	listed, err := root.ReadFile(schemasnapshot.File)
	if err != nil && !write {
		return err
	}
	if dsn == "" {
		return schemasnapshot.CheckMigrations(listed)
	}
	applied, err := schemasnapshot.Capture(ctx, dsn)
	if err != nil {
		return err
	}
	if write {
		return root.WriteFile(schemasnapshot.File, applied, 0o644)
	}
	if !bytes.Equal(listed, applied) {
		return schemasnapshot.ErrStale
	}
	return nil
}
