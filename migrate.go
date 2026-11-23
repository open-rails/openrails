package openrails

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/migrate"
)

// ErrMigrationsRequired means New, with startup migrations disabled, found a
// database Migrate has not prepared for this build: a migration is missing, or
// was applied with other content. The validation failure is preserved.
var ErrMigrationsRequired = migrate.ErrMigrationsRequired

// Migrate prepares OpenRails' schema, this month's partitions and River's
// tables in database's schemas, without constructing an engine. It runs
// regardless of database.SkipMigrations, which controls New only.
func Migrate(ctx context.Context, pool *pgxpool.Pool, database DatabaseConfig) error {
	return engine.Migrate(ctx, pool, Config{Database: database})
}
