package engine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/migrate"
)

// Migrate creates or upgrades through pool what New does first: OpenRails'
// tables in Config.Database.Schema and River's tables in
// Config.Database.RiverSchema. It is for operator tooling and fixtures that
// prepare a schema without running the engine.
func Migrate(ctx context.Context, pool *pgxpool.Pool, cfg billing.Config) error {
	if pool == nil {
		return fmt.Errorf("openrails: Migrate requires a Postgres pool")
	}
	schema := config.SchemaName(&cfg)
	if !validIdentifier(schema) {
		return fmt.Errorf("openrails: invalid database schema %q", schema)
	}
	riverSchema := config.RiverSchemaName(&cfg)
	if err := validRiverSchema(riverSchema); err != nil {
		return err
	}
	// Migrate takes no Deps: it logs to slog.Default().
	return migrate.Apply(ctx, pool, migrate.Options{Schema: schema, RiverSchema: riverSchema, Logger: logx.New(nil)})
}
