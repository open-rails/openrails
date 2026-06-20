package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
)

// newDataCmd wires the portable-data move CLI (#544): export/import the OpenRails
// PORTABLE core billing data so a deployment can switch between embedded and
// standalone (remote) modes without altering data.
//
//	openrails data export --out billing.dump
//	openrails data import --in billing.dump
//
// PORTABILITY CONTRACT (#544/#545): the exported set is exactly the OpenRails
// schema (config.DB.SchemaName(), default `openrails`) — and ONLY that. It is
// 100% portable because the two non-portable residents have been moved out:
//   - River job-queue tables (`river_*`) live in `public` (#545), not this schema.
//   - The migratekit ledger lives in `public.migrations`, not this schema.
//
// So a whole-schema dump needs no per-table exclusions. The standalone-only auth
// (AuthKit `profiles` schema) is NOT exported: it is recreated on
// embedded→standalone (see #543 authority model / `bootstrap apply`) and dropped
// on standalone→embedded.
//
// pg_dump/pg_restore are shelled out (the issue's "pg_dump-based" v1). A logical
// per-merchant JSON export is a planned later option for cross-PG-version moves.
func newDataCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "data",
		Short: "Move OpenRails portable billing data between deployments (#544): export/import the OpenRails schema",
	}
	cmd.AddCommand(newDataExportCmd(), newDataImportCmd())
	return cmd
}

func cfgFromCmd(c *cobra.Command) (*config.Config, error) {
	cfg, ok := c.Context().Value(config.ConfigContextKey).(*config.Config)
	if !ok || cfg == nil || cfg.DB == nil {
		return nil, fmt.Errorf("missing database configuration")
	}
	if cfg.DB.GetConnectionString() == "" {
		return nil, fmt.Errorf("no database connection configured (set db.url or db.host/port/database/username)")
	}
	return cfg, nil
}

func newDataExportCmd() *cobra.Command {
	var (
		out      string
		dataOnly bool
		format   string
	)
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Dump the OpenRails portable billing schema to a file (pg_dump)",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := cfgFromCmd(c)
			if err != nil {
				return err
			}
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			if err := requireBinary("pg_dump"); err != nil {
				return err
			}
			schema := cfg.DB.SchemaName()
			args := []string{
				"--dbname=" + cfg.DB.GetConnectionString(),
				"--schema=" + schema,
				"--no-owner",
				"--no-privileges",
				"--format=" + pgFormatFlag(format),
				"--file=" + out,
			}
			if dataOnly {
				args = append(args, "--data-only")
			}
			fmt.Fprintf(os.Stderr, "openrails data export: dumping schema %q -> %s (format=%s, data-only=%v)\n", schema, out, format, dataOnly)
			return runPGTool(c.Context(), "pg_dump", args)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "Output file path (required)")
	cmd.Flags().BoolVar(&dataOnly, "data-only", false, "Dump rows only (no DDL); use when importing into an already-migrated target")
	cmd.Flags().StringVar(&format, "format", "custom", "Dump format: custom (pg_restore) or plain (psql)")
	return cmd
}

func newDataImportCmd() *cobra.Command {
	var (
		in       string
		dataOnly bool
		format   string
		clean    bool
	)
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Load an OpenRails portable billing dump into the target DB (pg_restore)",
		Long: "Load a `data export` dump into the target database.\n\n" +
			"After importing into a STANDALONE target, run the auth-recreate step (#543) to\n" +
			"rebuild the AuthKit org(s) and admin authority — the dump carries billing data only.",
		RunE: func(c *cobra.Command, _ []string) error {
			cfg, err := cfgFromCmd(c)
			if err != nil {
				return err
			}
			if in == "" {
				return fmt.Errorf("--in is required")
			}
			if pgFormatFlag(format) == "p" {
				// Plain SQL dumps are replayed with psql, not pg_restore.
				if err := requireBinary("psql"); err != nil {
					return err
				}
				fmt.Fprintf(os.Stderr, "openrails data import: replaying %s via psql\n", in)
				return runPGTool(c.Context(), "psql", []string{"--dbname=" + cfg.DB.GetConnectionString(), "--set=ON_ERROR_STOP=1", "--file=" + in})
			}
			if err := requireBinary("pg_restore"); err != nil {
				return err
			}
			args := []string{
				"--dbname=" + cfg.DB.GetConnectionString(),
				"--no-owner",
				"--no-privileges",
				"--exit-on-error",
			}
			if dataOnly {
				// --disable-triggers defers FK checks during the data-only load: the
				// billing schema has circular foreign keys (e.g. the `grants` table),
				// which otherwise make a data-only restore order-dependent and fail.
				// Requires the restore role to be a superuser / table owner.
				args = append(args, "--data-only", "--disable-triggers")
			}
			if clean {
				// Drop existing objects before recreating them. Destructive — only
				// meaningful for a full (non-data-only) dump into a target you intend
				// to overwrite.
				args = append(args, "--clean", "--if-exists")
			}
			args = append(args, in)
			fmt.Fprintf(os.Stderr, "openrails data import: restoring %s (data-only=%v, clean=%v)\n", in, dataOnly, clean)
			return runPGTool(c.Context(), "pg_restore", args)
		},
	}
	cmd.Flags().StringVar(&in, "in", "", "Input dump file path (required)")
	cmd.Flags().BoolVar(&dataOnly, "data-only", false, "Restore rows only (target schema must already exist via `migrate up`)")
	cmd.Flags().StringVar(&format, "format", "custom", "Dump format: custom (pg_restore) or plain (psql)")
	cmd.Flags().BoolVar(&clean, "clean", false, "Drop existing objects before restore (full dumps only; destructive)")
	return cmd
}

func pgFormatFlag(format string) string {
	if format == "plain" || format == "p" {
		return "p"
	}
	return "c"
}

func requireBinary(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("%s not found on PATH (install the PostgreSQL client tools): %w", name, err)
	}
	return nil
}

// runPGTool runs a PostgreSQL client binary, streaming its stdout/stderr through.
// The connection string is passed via --dbname; in hardened deployments prefer
// libpq PG* env / a .pgpass file so credentials do not appear in argv.
func runPGTool(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}
