package migrate

import (
	"context"
	"database/sql"

	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/open-rails/authkit/authkitmigrate"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"

	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	log "github.com/sirupsen/logrus"
)

// RunPostgres applies all Postgres migrations:
// 0. bootstrap schema/extensions, 1. AuthKit (`profiles` schema), 2. River
// (`public`, #545), 3. OpenRails (billing schema).
func RunPostgres(ctx context.Context, cfg *config.Config) error {
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("missing database config")
	}

	// migratekit drives a database/sql handle; open one over the pgx stdlib
	// driver for the duration of the migration run.
	sqlDB, err := sql.Open("pgx", cfg.DB.GetConnectionString())
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	// Effective OpenRails schema (defaults to `openrails`). Validated as a safe
	// identifier during config load (#165).
	schema := cfg.DB.SchemaName()

	// ---------- 0. Bootstrap schema/extensions ----------
	if err := ensurePostgresBootstrap(ctx, sqlDB, schema); err != nil {
		return fmt.Errorf("postgres bootstrap failed: %w", err)
	}

	// ---------- 1. AuthKit Migrations (profiles schema) ----------
	log.Info("Running AuthKit migrations (profiles schema)...")
	authPool, err := db.NewPGXPoolWithRetry(ctx, cfg.DB.GetConnectionString())
	if err != nil {
		return fmt.Errorf("authkit: create pgx pool: %w", err)
	}
	defer authPool.Close()
	if _, err := authkitmigrate.New(authPool, &authkitmigrate.Config{}).Migrate(ctx); err != nil {
		return fmt.Errorf("authkit: apply migrations: %w", err)
	}
	log.Info("✓ AuthKit migrations completed successfully")

	// ---------- 2. River Migrations (always `public`, #545) ----------
	// River job-queue tables live in `public` (config.RiverSchema), NOT the
	// OpenRails billing schema, so the billing schema stays 100% portable for the
	// embedded↔standalone data move (#544). Must match the runtime client schema
	// (app.standaloneRiverSchema).
	log.Infof("Running River migrations (schema %q)...", config.RiverSchema)
	if err := runRiverMigrations(ctx, cfg, config.RiverSchema); err != nil {
		return fmt.Errorf("river migrations failed: %w", err)
	}

	// ---------- 3. OpenRails Migrations (OpenRails schema) ----------
	log.Infof("Running OpenRails migrations (schema %q)...", schema)
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	if err != nil {
		return fmt.Errorf("openrails: load migrations: %w", err)
	}

	// The migration DDL is authored schema-qualified to the default schema
	// (config.DefaultSchema, "openrails"). When a host configures a different
	// schema, relocate every qualifier before applying — search_path alone can't
	// move hard-qualified DDL (#471).
	migrations = rewriteMigrationsSchema(migrations, schema)

	// config.MigratekitApp is migratekit's app/tracking key
	// (public.migrations.app), independent of the schema (#471 renamed it from
	// "billing").
	m := migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(schema)
	// or#901: refuse a database that has run migrations this build no longer
	// carries, BEFORE applying anything. See assertNoOrphanedMigrations.
	if err := assertNoOrphanedMigrations(ctx, m, schema, migrations); err != nil {
		return err
	}
	// ApplyMigrations now calls Setup() automatically within the lock
	if err := m.ApplyMigrations(ctx, migrations); err != nil {
		return fmt.Errorf("openrails: apply migrations: %w", err)
	}
	log.Info("✓ OpenRails migrations completed successfully")
	return nil
}

// OrphanedMigrationsError reports that the database has recorded OpenRails
// migrations this build does not carry. It is always fatal: the schema and the
// binary describe different databases and nothing downstream can reconcile them.
type OrphanedMigrationsError struct {
	Schema   string
	Orphaned []string
	Embedded []string
}

func (e *OrphanedMigrationsError) Error() string {
	return fmt.Sprintf(
		"openrails: schema %q has applied migration(s) %s that this build does not carry (it carries %s) — "+
			"the recorded history is ahead of, or divergent from, the embedded set, so migratekit applies nothing "+
			"and the schema stays frozen while the binary advances. Rebuild the OpenRails schema from the current "+
			"baseline (see or#899's post-squash reset recipe); do NOT renumber migrations to paper over this",
		e.Schema, strings.Join(e.Orphaned, ", "), strings.Join(e.Embedded, ", "))
}

// assertNoOrphanedMigrations refuses when the database records an applied
// OpenRails migration that no longer exists in the embedded set.
//
// migratekit only ever asks "is every embedded migration applied?"
// (ApplyMigrations, ValidateAllApplied). It never asks the converse. A database
// that recorded 1..12 against a chain since re-squashed to {1, 2} therefore sees
// both remaining names already applied, applies nothing, and keeps the OLD
// schema — while the binary moves on to code written against the new baseline.
//
// That is exactly how or#901 lost the catalog reconciliation loop:
// psp_rail_merchant_ids was created by a numbered migration that or#893's
// re-squash absorbed into 0001, so on every already-migrated database the
// function was simply never created and both money-path reconcile jobs failed
// with SQLSTATE 42883 — silently, for 15 days, behind 25 retries apiece.
//
// The check is one-directional on purpose: PENDING migrations are ApplyMigrations'
// job, and are normal. ORPHANED ones are never normal.
func assertNoOrphanedMigrations(ctx context.Context, m *migratekit.Postgres, schema string, migrations []migratekit.Migration) error {
	applied, err := m.Applied(ctx)
	if err != nil {
		return fmt.Errorf("openrails: read applied migrations: %w", err)
	}
	embedded := make(map[string]struct{}, len(migrations))
	embeddedNames := make([]string, 0, len(migrations))
	for _, mig := range migrations {
		p := migratekit.Prefix(mig.Name)
		if _, seen := embedded[p]; seen {
			continue
		}
		embedded[p] = struct{}{}
		embeddedNames = append(embeddedNames, p)
	}

	var orphaned []string
	for _, name := range applied {
		if _, ok := embedded[name]; !ok {
			orphaned = append(orphaned, name)
		}
	}
	if len(orphaned) == 0 {
		return nil
	}
	sortMigrationNames(orphaned)
	sortMigrationNames(embeddedNames)
	return &OrphanedMigrationsError{
		Schema:   schema,
		Orphaned: orphaned,
		Embedded: embeddedNames,
	}
}

// sortMigrationNames orders migratekit prefixes numerically ("2" before "10"),
// falling back to lexical order for any non-numeric name.
func sortMigrationNames(names []string) {
	sort.Slice(names, func(i, j int) bool {
		ni, erri := strconv.ParseInt(names[i], 10, 64)
		nj, errj := strconv.ParseInt(names[j], 10, 64)
		if erri == nil && errj == nil {
			return ni < nj
		}
		if erri == nil {
			return true
		}
		if errj == nil {
			return false
		}
		return names[i] < names[j]
	})
}

// schemaWordRe matches the default schema name (config.DefaultSchema) as a whole
// word. The trailing \b means it does NOT match the unprivileged role name
// `openrails_app` (the underscore is a word character, so there is no boundary),
// while it DOES match both the `openrails.<table>` qualifier (the dot is a
// boundary) and bare schema-DDL references (`CREATE SCHEMA openrails`,
// `GRANT ... ON SCHEMA openrails`, `ALTER DEFAULT PRIVILEGES IN SCHEMA openrails`).
var schemaWordRe = regexp.MustCompile(`\b` + regexp.QuoteMeta(config.DefaultSchema) + `\b`)

// rewriteMigrationsSchema relocates every reference to the default schema in the
// migration DDL to the configured schema — both the hard-qualified
// `openrails.<table>` prefixes and the bare schema-name references in CREATE
// SCHEMA / GRANT ... ON SCHEMA / ALTER DEFAULT PRIVILEGES IN SCHEMA. It is a
// no-op when the schema is empty or already the default, so the common path pays
// nothing and runs the SQL verbatim. schema is a pre-validated SQL identifier
// (config.validateSchema), so the substitution can't inject anything unsafe.
//
// Prose/comments in the DDL say "OpenRails" (capitalized) or "billing-namespace"
// (the domain noun), neither of which the lowercase whole-word match touches.
func rewriteMigrationsSchema(migrations []migratekit.Migration, schema string) []migratekit.Migration {
	if schema == "" || schema == config.DefaultSchema {
		return migrations
	}
	out := make([]migratekit.Migration, len(migrations))
	for i, mig := range migrations {
		mig.Content = schemaWordRe.ReplaceAllString(mig.Content, schema)
		out[i] = mig
	}
	return out
}

// ensurePostgresBootstrap creates the OpenRails schema (configurable via
// db.schema, default `openrails` — #165/#471), shared extensions, and the
// migration tracking table. schema is a pre-validated SQL identifier
// (config.validateSchema), so it is safe to interpolate. CREATE SCHEMA IF NOT
// EXISTS is a no-op when the host already owns the schema.
func ensurePostgresBootstrap(ctx context.Context, db *sql.DB, schema string) error {
	if db == nil {
		return fmt.Errorf("missing sql db")
	}
	if schema == "" {
		schema = config.DefaultSchema
	}
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
		CREATE SCHEMA IF NOT EXISTS %s;
		CREATE EXTENSION IF NOT EXISTS pgcrypto WITH SCHEMA public;
		CREATE TABLE IF NOT EXISTS public.migrations (
			id BIGSERIAL PRIMARY KEY,
			app TEXT NOT NULL,
			database TEXT NOT NULL,
			name TEXT NOT NULL,
			migrated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE(app, database, name)
		);
	`, schema))
	return err
}

// Run applies all migrations (Postgres: River → Billing).
func Run(ctx context.Context, cfg *config.Config) error {
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("missing database config")
	}
	if err := RunPostgres(ctx, cfg); err != nil {
		return err
	}
	log.Info("✓ All migrations completed successfully")
	return nil
}

// runRiverMigrations executes River's built-in schema migrations
func runRiverMigrations(ctx context.Context, cfg *config.Config, schema string) error {
	pgxPool, err := db.NewPGXPoolWithRetry(ctx, cfg.DB.GetConnectionString())
	if err != nil {
		return fmt.Errorf("create pgx pool: %w", err)
	}
	defer pgxPool.Close()

	riverCfg := &rivermigrate.Config{}
	if schema != "" && schema != "public" {
		riverCfg.Schema = schema
	}

	migrator, err := rivermigrate.New(riverpgxv5.New(pgxPool), riverCfg)
	if err != nil {
		return fmt.Errorf("create River migrator: %w", err)
	}

	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("run River migrations: %w", err)
	}

	if len(res.Versions) == 0 {
		log.Info("No new River migrations to apply")
	} else {
		log.Infof("Applied %d River migration(s)", len(res.Versions))
	}

	return nil
}
