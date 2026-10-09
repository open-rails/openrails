package migrate

import (
	"context"
	"fmt"
	"io/fs"
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

// Options selects what Apply migrates.
type Options struct {
	// Schema holds OpenRails' tables; empty is billing.
	Schema string
	// RiverSchema holds River's tables; empty migrates no River.
	RiverSchema string
	// Chain is the migration files; nil is this build's.
	Chain fs.FS
}

// Apply creates or upgrades OpenRails' tables in Schema, this month's
// partitions, and River's tables in RiverSchema. The pool's role owns every
// object it creates. It is idempotent: concurrent callers serialize on
// advisory locks, and a migration already applied that Chain does not carry (a
// newer build's) is left as it is.
func Apply(ctx context.Context, pool *pgxpool.Pool, opts Options) error {
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
	chain := opts.Chain
	if chain == nil {
		chain = postgresmigrations.FS
	}
	render := func(schema string) ([]migratekit.Migration, error) { return loadChain(chain, schema) }

	log.Infof("Running OpenRails migrations (schema %q)...", schema)
	migrations, err := render(schema)
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
	m.WithSchema(schema).WithStrictIntegrity().WithRender(render)
	if err := m.ApplyMigrations(ctx, migrations); err != nil {
		return fmt.Errorf("openrails: apply migrations: %w", err)
	}
	// Partitions follow the calendar, not the migration chain: a database
	// migrated months ago still needs this month's. Replicas booting into a
	// new month would race to create the same ones.
	data, err := db.NewWithPGXPool(pool, schema)
	if err != nil {
		return err
	}
	if err := locked(ctx, pool, "openrails-partitions:"+schema, func() error {
		_, err := retention.EnsurePartitions(ctx, data.GenDirectory(), time.Now())
		return err
	}); err != nil {
		return fmt.Errorf("openrails: %w", err)
	}
	if opts.RiverSchema != "" {
		if err := applyRiver(ctx, pool, opts.RiverSchema); err != nil {
			return fmt.Errorf("openrails: River migrations in %s: %w", opts.RiverSchema, err)
		}
	}
	log.Info("✓ OpenRails migrations completed successfully")
	return nil
}

// loadMigrations returns this build's migrations relocated to schema.
func loadMigrations(schema string) ([]migratekit.Migration, error) {
	return loadChain(postgresmigrations.FS, schema)
}

// loadChain returns chain's migrations relocated to schema.
func loadChain(chain fs.FS, schema string) ([]migratekit.Migration, error) {
	migrations, err := migratekit.LoadFromFS(chain)
	if err != nil {
		return nil, fmt.Errorf("openrails: load migrations: %w", err)
	}
	return rewriteMigrationsSchema(migrations, schema)
}

// applyRiver migrates River's tables in schema, serialized with AuthKit and
// riverhelpers on the lock they share.
func applyRiver(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	return locked(ctx, pool, "river-migrations:"+schema, func() error {
		if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return err
		}
		// Always the declared namespace, even public: the caller's search_path
		// must not redirect River away from the schema locked.
		migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema})
		if err != nil {
			return fmt.Errorf("create River migrator: %w", err)
		}
		res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
		if err != nil {
			return fmt.Errorf("run River migrations: %w", err)
		}
		log.Infof("Applied %d River migration(s)", len(res.Versions))
		return nil
	})
}

// locked runs fn holding key's advisory lock on a session of its own, so the
// lock never pins a connection of the caller's pool.
func locked(ctx context.Context, pool *pgxpool.Pool, key string, fn func() error) error {
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())
	if err != nil {
		return fmt.Errorf("connect %s lock: %w", key, err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(cleanupCtx)
	}()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext(current_database()), hashtext($1))", key); err != nil {
		return fmt.Errorf("lock %s: %w", key, err)
	}
	return fn()
}
