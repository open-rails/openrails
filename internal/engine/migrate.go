package engine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// Migrate creates or upgrades through pool what New does: OpenRails' tables in
// Config.Database.Schema, this month's partitions, River's tables in
// Config.Database.RiverSchema and, with Config.ControlPlane, the control
// plane's AuthKit schema. It is for operator tooling and fixtures that
// prepare a schema without running the engine.
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
	if err := migrate.Apply(ctx, pool, migrate.Options{Schema: schema, RiverSchema: riverSchema}); err != nil {
		return err
	}
	if cfg.ControlPlane != nil {
		return standalonedb.ApplyAuthKit(ctx, pool, riverSchema)
	}
	return nil
}
