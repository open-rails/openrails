package migrate

import (
	"context"
	"database/sql"

	"fmt"
	"regexp"
	"strings"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	authkitpostgres "github.com/open-rails/authkit/migrations/postgres"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	clickhousemigrations "github.com/open-rails/openrails/migrations/clickhouse"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"

	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	log "github.com/sirupsen/logrus"
)

var replicatedReplacingMergeTreePattern = regexp.MustCompile(`ReplicatedReplacingMergeTree\([^)]*\{replica\}'(?:,\s*([A-Za-z_][A-Za-z0-9_]*))?\)`)

// RunPostgres applies all Postgres migrations for STANDALONE OpenRails:
// 0. bootstrap schema/extensions, 1. AuthKit (`profiles` schema), 2. River
// (`public`, #545), 3. OpenRails (billing schema). Embedded hosts call
// RunPostgresEmbedded, which skips the AuthKit step.
func RunPostgres(ctx context.Context, cfg *config.Config) error {
	return runPostgres(ctx, cfg, true)
}

// RunPostgresEmbedded applies the embedded subset — bootstrap + River + OpenRails
// — SKIPPING the AuthKit (`profiles`) migrations (#544). Embedded OpenRails runs
// no AuthKit; the host owns its identity layer. So `profiles` is the standalone-
// only "extra" table set: never created in embedded, which keeps the OpenRails
// schema the only portable set and "standalone has extra tables" literally true.
func RunPostgresEmbedded(ctx context.Context, cfg *config.Config) error {
	return runPostgres(ctx, cfg, false)
}

func runPostgres(ctx context.Context, cfg *config.Config, includeAuthKit bool) error {
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

	// ---------- 1. AuthKit Migrations (profiles schema) — STANDALONE ONLY ----------
	// Embedded OpenRails runs no AuthKit, so the `profiles` schema is never created
	// there; it is the standalone-only "extra" set thrown away / recreated on a
	// mode switch (#544).
	if includeAuthKit {
		log.Info("Running AuthKit migrations (profiles schema)...")
		authMigrations, err := migratekit.LoadFromFS(authkitpostgres.FS)
		if err != nil {
			return fmt.Errorf("authkit: load migrations: %w", err)
		}
		authMigrations = patchAuthKitAPIKeyRoleMigration(authMigrations)
		authMigrator := migratekit.NewPostgres(sqlDB, "authkit")
		if err := authMigrator.ApplyMigrations(ctx, authMigrations); err != nil {
			return fmt.Errorf("authkit: apply migrations: %w", err)
		}
		log.Info("✓ AuthKit migrations completed successfully")
	}

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
	// ApplyMigrations now calls Setup() automatically within the lock
	if err := m.ApplyMigrations(ctx, migrations); err != nil {
		return fmt.Errorf("openrails: apply migrations: %w", err)
	}
	log.Info("✓ OpenRails migrations completed successfully")
	return nil
}

func patchAuthKitAPIKeyRoleMigration(migrations []migratekit.Migration) []migratekit.Migration {
	const oldDDL = "ALTER TABLE profiles.service_tokens\n  ADD COLUMN role text NOT NULL;"
	const patchedDDL = `ALTER TABLE profiles.service_tokens
  ADD COLUMN IF NOT EXISTS role text;

INSERT INTO profiles.org_roles (org_id, role)
SELECT DISTINCT org_id, 'owner'
FROM profiles.service_tokens
WHERE role IS NULL
ON CONFLICT (org_id, role) DO NOTHING;

UPDATE profiles.service_tokens
SET role = 'owner'
WHERE role IS NULL;

ALTER TABLE profiles.service_tokens
  ALTER COLUMN role SET NOT NULL;`

	out := make([]migratekit.Migration, len(migrations))
	copy(out, migrations)
	for i := range out {
		if out[i].Name == "007_api_key_role.up.sql" {
			out[i].Content = strings.Replace(out[i].Content, oldDDL, patchedDDL, 1)
		}
	}
	return out
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

// RunClickHouse applies ClickHouse migrations
func RunClickHouse(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("missing config")
	}

	if cfg.ClickHouse == nil || cfg.ClickHouse.ClientAddr == "" {
		log.Info("ClickHouse not configured; skipping ClickHouse migrations")
		return nil
	}

	if cfg.DB == nil {
		return fmt.Errorf("missing database config (required for ClickHouse migration tracking/locking)")
	}

	// ClickHouse migrations are tracked/locked via Postgres (public.migrations + advisory locks).
	// ClickHouse migration tracking/locking rides a database/sql handle over
	// the pgx stdlib driver.
	sqlDB, err := sql.Open("pgx", cfg.DB.GetConnectionString())
	if err != nil {
		return fmt.Errorf("open db for ClickHouse migration tracking/locking: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	log.WithFields(log.Fields{
		"http_addr":   cfg.ClickHouse.HTTPAddr,
		"client_addr": cfg.ClickHouse.ClientAddr,
		"database":    cfg.ClickHouse.Database,
		"username":    cfg.ClickHouse.Username,
		"cluster":     cfg.ClickHouse.Cluster,
	}).Info("Running ClickHouse migrations")
	if err := runClickHouseMigrations(ctx, sqlDB, cfg.ClickHouse); err != nil {
		return fmt.Errorf("clickhouse migrations failed: %w", err)
	}

	log.Info("✓ ClickHouse migrations completed successfully")
	return nil
}

// Run applies all migrations (Postgres and ClickHouse independently):
// Postgres: River → Billing
// ClickHouse: Billing analytics
func Run(ctx context.Context, cfg *config.Config) error {
	if cfg == nil || cfg.DB == nil {
		return fmt.Errorf("missing database config")
	}

	// Run Postgres migrations
	pgErr := RunPostgres(ctx, cfg)

	// Run ClickHouse migrations independently (don't stop on Postgres failure)
	chErr := RunClickHouse(ctx, cfg)

	// Report results
	if pgErr != nil && chErr != nil {
		return fmt.Errorf("both migrations failed: postgres=%v; clickhouse=%v", pgErr, chErr)
	}
	if pgErr != nil {
		return pgErr
	}
	if chErr != nil {
		return chErr
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

// runClickHouseMigrations applies ClickHouse migrations using migratekit
func runClickHouseMigrations(ctx context.Context, sqlDB *sql.DB, cfg *config.ClickHouseConfig) error {
	chDB := cfg.Database
	if chDB == "" {
		chDB = "analytics"
	}

	chCluster := cfg.Cluster

	chMigrations, err := migratekit.LoadFromFS(clickhousemigrations.FS)
	if err != nil {
		return fmt.Errorf("clickhouse: load migrations: %w", err)
	}
	if chCluster == "" {
		chMigrations = useSingleNodeClickHouseEngines(chMigrations)
	}

	m := migratekit.NewClickHouse(&migratekit.ClickHouseConfig{
		ClientAddr: cfg.ClientAddr,
		Database:   chDB,
		Username:   cfg.Username,
		Password:   cfg.Password,
		App:        config.MigratekitApp,
		Cluster:    chCluster,
		PostgresDB: sqlDB,
	})
	// ApplyMigrations now calls Setup() automatically within the lock
	if err := m.ApplyMigrations(ctx, chMigrations); err != nil {
		return fmt.Errorf("clickhouse: apply migrations: %w", err)
	}

	log.Info("✓ ClickHouse migrations completed successfully")
	return nil
}

func useSingleNodeClickHouseEngines(migrations []migratekit.Migration) []migratekit.Migration {
	out := make([]migratekit.Migration, len(migrations))
	for i, migration := range migrations {
		migration.Content = replicatedReplacingMergeTreePattern.ReplaceAllStringFunc(migration.Content, func(engine string) string {
			match := replicatedReplacingMergeTreePattern.FindStringSubmatch(engine)
			if len(match) > 1 && match[1] != "" {
				return "ReplacingMergeTree(" + match[1] + ")"
			}
			return "ReplacingMergeTree()"
		})
		out[i] = migration
	}
	return out
}
