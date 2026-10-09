package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/migrate"
)

// Migrate creates or upgrades through pool what New does first: OpenRails'
// tables in Config.Database.Schema, this month's partitions and River's tables
// in Config.Database.RiverSchema. It is for operator tooling and fixtures that
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
	return migrate.Apply(ctx, pool, migrate.Options{Schema: schema, RiverSchema: riverSchema})
}

// AccessCutoverPreflight dry-runs, and with approvedBy approves, the cutover to
// product access in the schema cfg names (migrate.AccessCutoverPreflight).
func AccessCutoverPreflight(ctx context.Context, pool *pgxpool.Pool, cfg config.Config, approvedBy string) (billing.AccessCutoverReport, error) {
	if pool == nil {
		return billing.AccessCutoverReport{}, fmt.Errorf("openrails: AccessCutoverPreflight requires a Postgres pool")
	}
	schema := config.SchemaName(&cfg)
	if !validIdentifier(schema) {
		return billing.AccessCutoverReport{}, fmt.Errorf("openrails: invalid database schema %q", schema)
	}
	return migrate.AccessCutoverPreflight(ctx, pool, schema, strings.TrimSpace(approvedBy))
}
