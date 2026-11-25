package openrails

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/migrate"
)

// Migrate prepares OpenRails' schema and River's tables in database's
// schemas, without constructing an engine. It runs
// regardless of database.SkipMigrations, which controls New only.
func Migrate(ctx context.Context, pool *pgxpool.Pool, database billing.DatabaseConfig) error {
	return engine.Migrate(ctx, pool, billing.Config{Database: database}.WithDefaults())
}

// MigrateClickHouse creates or upgrades the event store's database on conn,
// as New does with Deps.ClickHouse: run it beside Migrate when
// billing.DatabaseConfig.SkipMigrations is set. It holds a PostgreSQL
// advisory lock on pool while it migrates, so instances migrating together
// take turns.
func MigrateClickHouse(ctx context.Context, pool *pgxpool.Pool, conn driver.Conn, clickhouse billing.ClickHouseConfig) error {
	if pool == nil {
		return errors.New("openrails: MigrateClickHouse requires a Postgres pool")
	}
	if conn == nil {
		return errors.New("openrails: MigrateClickHouse requires a ClickHouse connection")
	}
	cfg := billing.Config{ClickHouse: clickhouse}.WithDefaults()
	if err := config.ValidateClickHouse(cfg.ClickHouse); err != nil {
		return err
	}
	// MigrateClickHouse takes no Deps: it logs to slog.Default().
	database := config.ClickHouseDatabase(&cfg)
	return eventstore.Migrate(ctx, conn, database, eventstore.Options{Logger: logx.New(nil), Serialize: migrate.ClickHouseLock(pool, database)})
}
