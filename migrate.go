package openrails

import (
	"context"
	"errors"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/eventstore"
	"github.com/open-rails/openrails/internal/logx"
)

// Migrate prepares OpenRails' schema, this month's partitions and River's
// tables in database's schemas, without constructing an engine. It runs
// regardless of database.SkipMigrations, which controls New only.
func Migrate(ctx context.Context, pool *pgxpool.Pool, database DatabaseConfig) error {
	return engine.Migrate(ctx, pool, config.Config{Database: config.DatabaseConfig(database)})
}

// MigrateClickHouse creates or upgrades the event store's database on conn,
// as New does with Deps.ClickHouse: run it beside Migrate when
// DatabaseConfig.SkipMigrations is set.
func MigrateClickHouse(ctx context.Context, conn driver.Conn, clickhouse ClickHouseConfig) error {
	if conn == nil {
		return errors.New("openrails: MigrateClickHouse requires a ClickHouse connection")
	}
	cfg := config.Config{ClickHouse: config.ClickHouseConfig(clickhouse)}
	if err := config.ValidateClickHouse(cfg.ClickHouse); err != nil {
		return err
	}
	// MigrateClickHouse takes no Deps: it logs to slog.Default().
	return eventstore.Migrate(ctx, conn, config.ClickHouseDatabase(&cfg), logx.New(nil))
}
