package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/retention"
	"github.com/open-rails/openrails/internal/sqlschema"

	riverpgxv5 "github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// Options selects what Apply migrates.
type Options struct {
	// Schema holds OpenRails' tables; empty is billing.
	Schema string
	// RiverSchema holds River's tables; empty migrates no River.
	RiverSchema string
	// Chain is the migration files; nil is this build's.
	Chain fs.FS
	// Logger receives the migrations' progress and migratekit's and River's
	// warnings; required.
	Logger *slog.Logger
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
	logger := opts.Logger
	if logger == nil {
		return fmt.Errorf("migrate: a logger is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	schema := opts.Schema
	if schema == "" {
		schema = sqlschema.Authored
	}
	chain := opts.Chain
	if chain == nil {
		chain = postgresmigrations.FS
	}
	render := func(schema string) ([]migratekit.Migration, error) { return loadChain(chain, schema) }

	logger.InfoContext(ctx, "running OpenRails migrations", "schema", schema)
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
	m.WithSchema(schema).WithStrictIntegrity().WithRender(render).WithWarnFunc(func(d migratekit.Discrepancy) {
		logger.WarnContext(ctx, "migratekit: "+d.OneLine(), "kind", string(d.Kind), "migration", d.File, "key", d.Key)
	})
	if err := m.ApplyMigrations(ctx, migrations); err != nil {
		return fmt.Errorf("openrails: apply migrations: %w", err)
	}
	status, err := m.Status(ctx, migrations)
	if err != nil {
		return fmt.Errorf("openrails: migration status: %w", err)
	}
	contracts, others := newerMigrations(status.Applied, migrations)
	if len(contracts) > 0 {
		logger.WarnContext(ctx, "OpenRails schema has contract migrations from a newer build: this build runs only if it no longer uses what they removed", "schema", schema, "migrations", strings.Join(contracts, ", "))
	}
	if len(others) > 0 {
		logger.InfoContext(ctx, "OpenRails schema carries migrations from a newer build", "schema", schema, "migrations", strings.Join(others, ", "))
	}
	// Partitions follow the calendar, not the migration chain: a database
	// migrated months ago still needs this month's.
	data, err := db.NewWithPGXPool(logger, pool, schema)
	if err != nil {
		return err
	}
	if _, err := retention.EnsurePartitions(ctx, data.GenDirectory(), time.Now()); err != nil {
		return fmt.Errorf("openrails: %w", err)
	}
	if opts.RiverSchema != "" {
		if err := applyRiver(ctx, logger, pool, opts.RiverSchema); err != nil {
			return fmt.Errorf("openrails: River migrations in %s: %w", opts.RiverSchema, err)
		}
	}
	logger.InfoContext(ctx, "OpenRails migrations applied", "schema", schema)
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
func applyRiver(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, schema string) error {
	return locked(ctx, pool, "river-migrations:"+schema, func() error {
		if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{schema}.Sanitize()); err != nil {
			return err
		}
		// Always the declared namespace, even public: the caller's search_path
		// must not redirect River away from the schema locked.
		migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema, Logger: logger})
		if err != nil {
			return fmt.Errorf("create River migrator: %w", err)
		}
		res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil)
		if err != nil {
			return fmt.Errorf("run River migrations: %w", err)
		}
		logger.InfoContext(ctx, "River migrations applied", "schema", schema, "count", len(res.Versions))
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
