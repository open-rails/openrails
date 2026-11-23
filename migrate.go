package openrails

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
)

// Migrate prepares OpenRails' schema, this month's partitions and River's
// tables in database's schemas, without constructing an engine. It runs
// regardless of database.SkipMigrations, which controls New only.
func Migrate(ctx context.Context, pool *pgxpool.Pool, database DatabaseConfig) error {
	return engine.Migrate(ctx, pool, config.Config{Database: config.DatabaseConfig(database)})
}
