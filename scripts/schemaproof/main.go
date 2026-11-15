// schemaproof proves that the working tree's migrations build exactly the
// database another revision's migrations build. It applies both chains through
// migrate.Apply to scratch databases, in the default schema and in another one
// (through the schema rewriter), and requires identical pg_dump output: schema
// and data with privileges, without owners. pg_dump orders objects itself, so
// the files' own layout does not matter.
//
//	go run ./scripts/schemaproof [-ref origin/master] [-dsn URL] [-pg-dump COMMAND]
//
// -dsn (or OPENRAILS_E2E_DSN) names a disposable PostgreSQL 18 server whose
// role may create databases. -pg-dump (or PG_DUMP) is the pg_dump 18 command,
// "pg_dump" on PATH by default; CI runs the server container's
// ("docker exec critical-postgres pg_dump"), which must reach the server at
// the DSN's address.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/schemasnapshot"
)

const dir = "internal/migrate/postgres"

// otherSchema is where the rewriter relocates both chains.
const otherSchema = "schemaproof"

func main() {
	ref := flag.String("ref", "origin/master", "revision whose migration chain is the reference")
	dsn := flag.String("dsn", os.Getenv(schemasnapshot.DSNEnv), "disposable PostgreSQL 18 server")
	keep := flag.String("keep", "", "directory to keep the dumps in")
	pgDumpCommand := "pg_dump"
	if v := strings.TrimSpace(os.Getenv("PG_DUMP")); v != "" {
		pgDumpCommand = v
	}
	flag.StringVar(&pgDumpCommand, "pg-dump", pgDumpCommand, "the pg_dump 18 command, split on spaces")
	flag.Parse()
	if pgDump = strings.Fields(pgDumpCommand); len(pgDump) == 0 {
		pgDump = []string{"pg_dump"}
	}
	if err := run(context.Background(), *ref, strings.TrimSpace(*dsn), *keep); err != nil {
		fmt.Fprintln(os.Stderr, "schemaproof:", err)
		os.Exit(1)
	}
}

// pgDump is the pg_dump command and its leading arguments.
var pgDump []string

func run(ctx context.Context, ref, dsn, keep string) error {
	if dsn == "" {
		return fmt.Errorf("-dsn or %s must name a disposable PostgreSQL server", schemasnapshot.DSNEnv)
	}
	old, oldCount, err := chainAt(ctx, ref)
	if err != nil {
		return err
	}
	tree := os.DirFS(dir)
	names, err := fs.Glob(tree, "*.up.sql")
	if err != nil {
		return err
	}
	out := keep
	if out == "" {
		if out, err = os.MkdirTemp("", "schemaproof-"); err != nil {
			return err
		}
		defer os.RemoveAll(out)
	} else if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	fmt.Printf("reference: %s at %s (%d files); candidate: %s in the working tree (%d files)\n", dir, ref, oldCount, dir, len(names))
	for _, schema := range []string{config.DefaultSchema, otherSchema} {
		want, err := dump(ctx, dsn, schema, old)
		if err != nil {
			return fmt.Errorf("%s chain in %s: %w", ref, schema, err)
		}
		got, err := dump(ctx, dsn, schema, tree)
		if err != nil {
			return fmt.Errorf("working tree in %s: %w", schema, err)
		}
		wantFile, gotFile := filepath.Join(out, schema+".reference.sql"), filepath.Join(out, schema+".candidate.sql")
		if err := os.WriteFile(wantFile, want, 0o644); err != nil { // #nosec G703 -- the operator's -keep directory
			return err
		}
		if err := os.WriteFile(gotFile, got, 0o644); err != nil { // #nosec G703 -- the operator's -keep directory
			return err
		}
		if !bytes.Equal(want, got) {
			diff, _ := exec.CommandContext(ctx, "diff", "-u", wantFile, gotFile).CombinedOutput()
			os.Stdout.Write(diff)
			return fmt.Errorf("schema %s: the dumps differ", schema)
		}
		if schema != config.DefaultSchema {
			if left := defaultRef.FindAll(quoted.ReplaceAll(got, nil), -1); len(left) > 0 {
				return fmt.Errorf("schema %s: %d references to %s survive the rewriter", schema, len(left), config.DefaultSchema)
			}
		}
		fmt.Printf("schema %s: identical, %d lines (%d bytes)\n", schema, bytes.Count(got, []byte("\n")), len(got))
	}
	return nil
}

// A reference to the default schema left outside a string literal is one the
// rewriter missed.
var (
	defaultRef = regexp.MustCompile(`\b` + config.DefaultSchema + `\.\w`)
	quoted     = regexp.MustCompile(`'(?:[^']|'')*'`)
)

// chainAt reads ref's migration files from git.
func chainAt(ctx context.Context, ref string) (fs.FS, int, error) {
	list, err := exec.CommandContext(ctx, "git", "ls-tree", "--name-only", ref, dir+"/").Output()
	if err != nil {
		return nil, 0, fmt.Errorf("list %s at %s: %w", dir, ref, err)
	}
	chain := fstest.MapFS{}
	for _, name := range strings.Fields(string(list)) {
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		body, err := exec.CommandContext(ctx, "git", "show", ref+":"+name).Output()
		if err != nil {
			return nil, 0, fmt.Errorf("read %s at %s: %w", name, ref, err)
		}
		chain[path.Base(name)] = &fstest.MapFile{Data: body}
	}
	if len(chain) == 0 {
		return nil, 0, fmt.Errorf("%s has no migrations at %s", dir, ref)
	}
	return chain, len(chain), nil
}

// dump applies chain to a scratch database in schema and returns its
// normalised pg_dump.
func dump(ctx context.Context, dsn, schema string, chain fs.FS) ([]byte, error) {
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	defer admin.Close(context.WithoutCancel(ctx))
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	name := "schemaproof_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return nil, err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()
	target, err := url.Parse(dsn)
	if err != nil {
		return nil, err
	}
	target.Path = "/" + name
	pool, err := pgxpool.New(ctx, target.String())
	if err != nil {
		return nil, err
	}
	err = migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Chain: chain})
	var oids [2]string
	if err == nil {
		err = pool.QueryRow(ctx, `SELECT d.oid::text, n.oid::text FROM pg_database d, pg_namespace n WHERE d.datname = current_database() AND n.nspname = $1`, schema).Scan(&oids[0], &oids[1])
	}
	pool.Close()
	if err != nil {
		return nil, err
	}
	args := append(pgDump[1:len(pgDump):len(pgDump)], "--dbname="+target.String(), "--schema="+schema, "--no-owner")
	raw, err := exec.CommandContext(ctx, pgDump[0], args...).Output() // #nosec G702 -- the operator's -pg-dump command
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%s: %w: %s", strings.Join(pgDump, " "), err, bytes.TrimSpace(exit.Stderr))
		}
		return nil, fmt.Errorf("%s: %w", strings.Join(pgDump, " "), err)
	}
	return normalise(raw, oids), nil
}

// normalise drops what pg_dump varies between databases or runs: its header
// comments, session settings and the \restrict key; in rows, the instants,
// UUIDs and the database's and schema's oids a migration recorded.
func normalise(raw []byte, oids [2]string) []byte {
	var out bytes.Buffer
	rows := false
	for line := range strings.Lines(string(raw)) {
		bare := strings.TrimSuffix(line, "\n")
		switch {
		case rows && bare == `\.`:
			rows = false
		case rows:
			fields := strings.Split(bare, "\t")
			for i, f := range fields {
				switch {
				case f == oids[0]:
					fields[i] = "<database oid>"
				case f == oids[1]:
					fields[i] = "<schema oid>"
				case instant.MatchString(f):
					fields[i] = "<instant>"
				case uuidValue.MatchString(f):
					fields[i] = "<uuid>"
				}
			}
			line = strings.Join(fields, "\t") + "\n"
		case strings.HasPrefix(bare, "COPY ") && strings.HasSuffix(bare, " FROM stdin;"):
			rows = true
		case bare == "--",
			strings.HasPrefix(bare, "-- Name: "),
			strings.HasPrefix(bare, "-- Data for Name: "),
			strings.HasPrefix(bare, "-- PostgreSQL database dump"),
			strings.HasPrefix(bare, "-- Dumped "),
			strings.HasPrefix(bare, "SET "),
			strings.HasPrefix(bare, "SELECT pg_catalog.set_config('search_path'"),
			strings.HasPrefix(bare, `\restrict `),
			strings.HasPrefix(bare, `\unrestrict `):
			continue
		}
		out.WriteString(line)
	}
	return out.Bytes()
}

var (
	instant   = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(\.\d+)?\+00$`)
	uuidValue = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)
