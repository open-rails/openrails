// contracts verifies or rewrites the reviewed contracts: the Go API list
// (api/go.txt), the schema list (api/schema.txt), the Go wire snapshot
// (compatibility/contract.json) and the files generated from the route
// catalog (the Go Client in client_gen.go, api/openapi.json, the TypeScript
// wire types, the route and error-code tables in docs/api). The generated
// files come first: the Go API is read from the package they complete.
//
// The schema list is read from a real PostgreSQL: -dsn, or OPENRAILS_E2E_DSN,
// names a disposable server. Without one the list is only checked against the
// migration files, and -write refuses to leave it behind them.
//
// -lint instead checks the Go API against its conventions and the findings
// baseline (api/lint-baseline.txt), needing no database; -lint -prune drops
// the baseline lines that no longer match.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/open-rails/openrails/internal/apisurface"
	"github.com/open-rails/openrails/internal/contract"
	"github.com/open-rails/openrails/internal/contractaudit"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/schemasnapshot"
)

func main() {
	write := flag.Bool("write", false, "write the reviewed current contracts")
	dsn := flag.String("dsn", os.Getenv(schemasnapshot.DSNEnv), "disposable PostgreSQL server the schema list is read from")
	lint := flag.Bool("lint", false, "check the Go API against its conventions and "+apisurface.BaselineFile)
	prune := flag.Bool("prune", false, "with -lint, drop the baseline lines that match nothing")
	flag.Parse()
	var err error
	switch {
	case *prune && !*lint:
		err = fmt.Errorf("-prune needs -lint")
	case *lint:
		err = lintAPI(*prune)
	default:
		err = run(*write, strings.TrimSpace(*dsn))
	}
	if err != nil {
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
	if !write {
		if err := contract.Verify(root.FS()); err != nil {
			return err
		}
		surface, err := apisurface.Load(ctx, ".")
		if err != nil {
			return err
		}
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
		return schema(ctx, root, dsn, false)
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
	surface, err := apisurface.Load(ctx, ".")
	if err != nil {
		return err
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
	return schema(ctx, root, dsn, true)
}

// lintAPI checks the Go API's findings against the baseline. prune rewrites
// the baseline without its stale lines; it never adds one.
func lintAPI(prune bool) error {
	root, err := os.OpenRoot(".")
	if err != nil {
		return err
	}
	defer root.Close()
	surface, err := apisurface.Load(context.Background(), ".")
	if err != nil {
		return err
	}
	findings, err := surface.Lint()
	if err != nil {
		return err
	}
	baseline, err := root.ReadFile(apisurface.BaselineFile)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	kept, err := apisurface.Compare(findings, baseline)
	if prune && baseline != nil {
		if len(kept) == 0 {
			if err := root.Remove(apisurface.BaselineFile); err != nil {
				return err
			}
		} else if err := root.WriteFile(apisurface.BaselineFile, apisurface.Baseline(kept), 0o644); err != nil {
			return err
		}
		// Pruned: only findings the baseline never listed still fail.
		_, err = apisurface.Compare(findings, apisurface.Baseline(kept))
	}
	if err == nil {
		fmt.Printf("%s: %d findings left\n", apisurface.BaselineFile, len(kept))
	}
	return err
}

// schema checks or rewrites api/schema.txt. With no server it can only tell
// whether the list was taken from the migration files as they are now.
func schema(ctx context.Context, root *os.Root, dsn string, write bool) error {
	listed, err := root.ReadFile(schemasnapshot.File)
	if err != nil && !write {
		return err
	}
	if dsn == "" {
		if err := inventory(listed); err != nil {
			return err
		}
		return schemasnapshot.CheckMigrations(listed)
	}
	applied, err := schemasnapshot.Capture(ctx, dsn)
	if err != nil {
		return err
	}
	if err := inventory(applied); err != nil {
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

var schemaTable = regexp.MustCompile(`(?m)^table billing\.(\w+)`)

// inventory requires the merchant archive's table inventory to name exactly
// the schema list's tables: the archive judges only its inventory, so a table
// a migration adds without an inventory decision would otherwise be left out
// of the export.
func inventory(schema []byte) error {
	var tables []string
	for _, m := range schemaTable.FindAllSubmatch(schema, -1) {
		tables = append(tables, string(m[1]))
	}
	owned := merchantarchive.OwnedTables()
	for _, name := range tables {
		if !slices.Contains(owned, name) {
			return fmt.Errorf("%s lists billing.%s, which the merchant archive inventory does not name: decide whether the archive carries it", schemasnapshot.File, name)
		}
	}
	for _, name := range owned {
		if !slices.Contains(tables, name) {
			return fmt.Errorf("the merchant archive inventory names %s, which %s does not list", name, schemasnapshot.File)
		}
	}
	return nil
}
