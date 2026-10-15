package openrails

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/migrate"
)

// ErrMigrationsRequired means New found an unprepared database while startup
// migrations were disabled. The underlying validation failure is preserved.
var ErrMigrationsRequired = migrate.ErrMigrationsRequired

// Migrate explicitly prepares OpenRails' schema, initial partitions and River
// tables without constructing a runtime. It runs regardless of SkipMigrations,
// which controls automatic application startup only.
func Migrate(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	return engine.Migrate(ctx, pool, cfg)
}
