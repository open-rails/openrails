package migrate

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/config"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"

	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
	log "github.com/sirupsen/logrus"
)

// Options selects the OpenRails schema and the River schema it manages.
// HostRiver leaves River's migrations to the host.
type Options struct {
	Schema      string
	RiverSchema string
	HostRiver   bool
}

// ApplyPostgresMigrations applies OpenRails' embedded migrations, then River's
// unless the host owns River. The pool's role owns every object it creates and
// is the role OpenRails runs as. AuthKit migrates its own schema.
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
	riverSchema := opts.RiverSchema
	if riverSchema == "" {
		riverSchema = config.RiverSchema
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
	if !opts.HostRiver {
		if err := runRiverMigrationsPool(ctx, pool, riverSchema); err != nil {
			return fmt.Errorf("river migrations failed: %w", err)
		}
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
