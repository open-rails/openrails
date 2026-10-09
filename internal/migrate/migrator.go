package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/retention"

	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	log "github.com/sirupsen/logrus"
)

// Options selects the OpenRails schema.
type Options struct {
	Schema string
}

// ApplyPostgresMigrations applies OpenRails' embedded migrations. The pool's
// role owns every object it creates and is the role OpenRails runs as. River
// migrates separately (ApplyRiver); AuthKit migrates its own schema.
func ApplyPostgresMigrations(ctx context.Context, pool *pgxpool.Pool, opts Options) error {
	if pool == nil {
		return fmt.Errorf("missing postgres pool")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	schema := opts.Schema
	if schema == "" {
		schema = config.DefaultSchema
	}

	log.Infof("Running OpenRails migrations (schema %q)...", schema)
	migrations, err := loadMigrations(schema)
	if err != nil {
		return err
	}
	// Own sessions, so the advisory lock never waits on the host pool.
	m, err := migratekit.NewPostgresFromPGXPool(pool, config.MigratekitApp)
	if err != nil {
		return fmt.Errorf("create OpenRails migrator: %w", err)
	}
	defer m.Close()
	// Strict integrity: an applied migration whose content changed is refused
	// unless the live schema still equals a fresh build of it.
	m.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	if err := m.ApplyMigrations(ctx, migrations); err != nil {
		return fmt.Errorf("openrails: apply migrations: %w", err)
	}
	// Partitions follow the calendar, not the migration chain: a database
	// migrated months ago still needs this month's.
	data, err := db.NewWithPGXPool(pool, schema)
	if err != nil {
		return err
	}
	if _, err := retention.EnsurePartitions(ctx, data.GenDirectory(), time.Now()); err != nil {
		return fmt.Errorf("openrails: %w", err)
	}
	log.Info("✓ OpenRails migrations completed successfully")
	return nil
}

// loadMigrations returns the embedded migrations relocated to schema.
func loadMigrations(schema string) ([]migratekit.Migration, error) {
	migrations, err := migratekit.LoadFromFS(postgresmigrations.FS)
	if err != nil {
		return nil, fmt.Errorf("openrails: load migrations: %w", err)
	}
	return rewriteMigrationsSchema(migrations, schema)
}

// ApplyRiver migrates River's tables in schema.
func ApplyRiver(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if err := runRiverMigrationsPool(ctx, pool, schema); err != nil {
		return fmt.Errorf("openrails: River migrations in %s: %w", schema, err)
	}
	return nil
}

// runRiverMigrationsPool executes River's built-in schema migrations over the
// caller's pool, serialized with AuthKit and riverhelpers on one lock.
func runRiverMigrationsPool(ctx context.Context, pgxPool *pgxpool.Pool, schema string) error {
	if pgxPool == nil {
		return fmt.Errorf("missing postgres pool")
	}
	if schema == "" {
		return fmt.Errorf("River schema is required")
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
