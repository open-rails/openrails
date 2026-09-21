package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"

	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	log "github.com/sirupsen/logrus"
)

// RunPostgres applies OpenRails' own Postgres migrations and its River schema.
// AuthKit is a separate dependency and owns its own schema migrations.
func RunPostgres(ctx context.Context, cfg *config.Config) error {
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("missing database config")
	}

	pool, err := db.NewPGXPoolWithRetry(ctx, cfg.DB.GetConnectionString())
	if err != nil {
		return fmt.Errorf("open db: %w", err)
	}
	defer pool.Close()
	return ApplyPostgresMigrations(ctx, pool, Options{Schema: cfg.DB.SchemaName()})
}

// Options selects the billing namespace and managed River namespace.
// HostRiver leaves all River migrations and privileges to the host.
type Options struct {
	Schema      string
	RiverSchema string
	HostRiver   bool
	RuntimePool *pgxpool.Pool
}

// ApplyPostgresMigrations applies OpenRails' embedded billing migrations and
// the River migrations it owns. The pool must use a role permitted to create
// the configured schema, extensions, and RLS policy objects. AuthKit's
// profiles schema is deliberately outside this package: callers initialize it
// through AuthKit's own embedded migration API.
func ApplyPostgresMigrations(ctx context.Context, pool *pgxpool.Pool, opts Options) error {
	if pool == nil {
		return fmt.Errorf("missing postgres pool")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	schema := opts.Schema
	// Effective OpenRails schema (defaults to `billing`). Validated as a safe
	// identifier during config load (#165).
	if schema == "" {
		schema = config.DefaultSchema
	}

	riverSchema := opts.RiverSchema
	if riverSchema == "" {
		riverSchema = config.RiverSchema
	}
	if !opts.HostRiver && riverSchema == schema {
		return fmt.Errorf("River schema must differ from billing schema")
	}

	var runtimeUser string
	if opts.RuntimePool != nil {
		var err error
		runtimeUser, err = runtimeLogin(ctx, pool, opts.RuntimePool)
		if err != nil {
			return fmt.Errorf("runtime access: %w", err)
		}
	}

	// Migratekit creates the schema under its lock; the baseline migration owns
	// extensions. Preliminary DDL outside that lock would race concurrent boots.
	log.Infof("Running OpenRails migrations (schema %q)...", schema)
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	if err != nil {
		return fmt.Errorf("openrails: load migrations: %w", err)
	}

	// The migration DDL is authored schema-qualified to the default schema
	// (config.CanonicalSchema, "openrails"). When a host configures a different
	// schema, relocate every qualifier before applying — search_path alone can't
	// move hard-qualified DDL (#471).
	migrations, err = rewriteMigrationsSchema(migrations, schema)
	if err != nil {
		return err
	}

	// config.MigratekitApp is migratekit's app/tracking key
	// (public.migrations.app), independent of the schema (#471 renamed it from
	// "billing").
	// Own migration sessions instead of borrowing a host connection for the
	// advisory lock while waiting for another from the same bounded pool.
	m, err := migratekit.NewPostgresFromPGXPool(pool, config.MigratekitApp)
	if err != nil {
		return fmt.Errorf("create OpenRails migrator: %w", err)
	}
	defer m.Close()
	m.WithSchema(schema)
	// or#901: refuse a database that has run migrations this build no longer
	// carries, BEFORE applying anything. See assertNoOrphanedMigrations.
	// A fresh database has no ledger yet. Applied only reads it; migratekit's
	// ApplyMigrations owns creating it under its bootstrap lock.
	var ledgerExists bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('public.migrations') IS NOT NULL").Scan(&ledgerExists); err != nil {
		return fmt.Errorf("inspect migration ledger: %w", err)
	}
	if ledgerExists {
		if err := assertNoOrphanedMigrations(ctx, m, schema, migrations); err != nil {
			return err
		}
	}
	// ApplyMigrations initializes the migration tracker automatically.
	if err := m.ApplyMigrations(ctx, migrations); err != nil {
		return fmt.Errorf("openrails: apply migrations: %w", err)
	}
	if !opts.HostRiver {
		if err := runRiverMigrationsPool(ctx, pool, riverSchema); err != nil {
			return fmt.Errorf("river migrations failed: %w", err)
		}
	}
	if opts.RuntimePool != nil {
		if err := provisionRuntimeAccess(ctx, pool, runtimeUser, schema, riverSchema, opts.HostRiver); err != nil {
			return fmt.Errorf("runtime access: %w", err)
		}
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

// AssertNoOrphanedPostgresMigrations is the drift fence at the seam EVERY
// runtime crosses — not just the one the CLI takes.
//
// or#901 installed the check inside RunPostgres, which only the standalone
// binary calls. An embedded host applies the migratekit chain itself
// (host-four's runOpenRailsMigrations) and relies on the engine's own
// init-time validation, so the fence protected exactly the deployment that
// was never frozen. upstream#1627 is the bill: host-four's dev stack sat on the
// pre-squash schema, `billing_policies` did not exist, and every billed
// admission answered 500 while both ValidatePostgresMigrations and
// ApplyMigrations reported success.
//
// sqlDB is any handle on the target database; schema is the effective
// OpenRails schema (cfg.DB.SchemaName()).
func AssertNoOrphanedPostgresMigrations(ctx context.Context, sqlDB *sql.DB, schema string) error {
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	if err != nil {
		return fmt.Errorf("openrails: load migrations: %w", err)
	}
	// Names are filename prefixes, so the schema rewrite (DDL text only) is
	// irrelevant here — only WithSchema matters, since migratekit filters the
	// ledger by it.
	m := migratekit.NewPostgres(sqlDB, config.MigratekitApp).WithSchema(schema)
	return assertNoOrphanedMigrations(ctx, m, schema, migrations)
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

// Run applies all OpenRails-owned migrations (billing and managed River).
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

// runRiverMigrationsPool executes River's built-in schema migrations over the
// caller's pool. Keeping this in the OpenRails migration package means the
// consumer never needs to import rivermigrate either.
func runRiverMigrationsPool(ctx context.Context, pgxPool *pgxpool.Pool, schema string) error {
	if pgxPool == nil {
		return fmt.Errorf("missing postgres pool")
	}
	if schema == "" {
		schema = config.RiverSchema
	}
	// Shared protocol with AuthKit: serialize schema creation and River's own
	// version migrations without pinning the caller pool's only connection.
	lockConn, err := pgx.ConnectConfig(ctx, pgxPool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect River migration lock: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = lockConn.Close(cleanupCtx)
	}()
	if _, err := lockConn.Exec(ctx, "SELECT pg_advisory_lock(hashtext(current_database()), hashtext($1))", "river-migrations:"+schema); err != nil {
		return fmt.Errorf("lock River migrations: %w", err)
	}
	if _, err := pgxPool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return err
	}
	// Always target the declared namespace, including public; caller search_path
	// must not redirect River migrations away from the namespace we locked.
	riverCfg := &rivermigrate.Config{Schema: schema}

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
