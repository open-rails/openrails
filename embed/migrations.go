package embed

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/migrate"
)

var migrationSchemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// MigrationOptions selects the billing namespace and River ownership. Use the
// same Schema and River values in the runtime configuration and Options.River.
// The zero value creates billing tables in billing and manages River in public.
type MigrationOptions struct {
	Schema string
	River  RiverOwnership
}

// ApplyMigrations initializes OpenRails-owned database objects using a privileged
// pool. Hosts never import migration files or construct a River migrator. With
// RiverFromHost, River migration and access policy remain entirely host-owned.
// RiverFromHost(nil) is sufficient here; only New needs the live client binder.
// AuthKit is initialized separately through its own embedded migration API.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, opts MigrationOptions) error {
	if pool == nil {
		return fmt.Errorf("openrails embed: postgres pool is required")
	}
	schema, err := validateMigrationSchema(opts.Schema)
	if err != nil {
		return err
	}
	riverSchema, err := opts.River.managedSchema(schema)
	if err != nil {
		return err
	}
	return migrate.ApplyPostgresMigrations(ctx, pool, migrate.Options{Schema: schema, RiverSchema: riverSchema, HostRiver: opts.River.host})
}

func validateMigrationSchema(schema string) (string, error) {
	schema = strings.ToLower(strings.TrimSpace(schema))
	if schema == "" {
		schema = config.DefaultSchema
	}
	if len(schema) > 63 || !migrationSchemaName.MatchString(schema) {
		return "", fmt.Errorf("openrails embed: invalid database schema %q", schema)
	}
	return schema, nil
}
