package engine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/migrate"
)

// Migrate creates or upgrades through pool what New does first: OpenRails'
// tables in Config.Database.Schema, this month's partitions and River's tables
// in Config.Database.RiverSchema. It is for operator tooling and fixtures that
// prepare a schema without running the engine. The control plane's AuthKit
// migrates itself when New builds it.
func Migrate(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) error {
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
	return migrate.Apply(ctx, pool, migrate.Options{Schema: schema, RiverSchema: riverSchema})
}
