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

// ApplyMigrations initializes the database objects owned by OpenRails.
//
// OpenRails loads and applies its embedded billing migrations and the River
// migrations used by RiverManagedByOpenRails. The caller supplies a privileged
// pool because initialization creates the configured billing schema, shared
// extensions, RLS roles/policies, and River's tables. The caller does not need
// migratekit, rivermigrate, or an OpenRails migration package.
//
// AuthKit owns its profiles schema independently; initialize it through
// authkit/embedded.ApplyMigrations before constructing the AuthKit client. A
// blank schema selects OpenRails' default schema, "openrails".
//
// RiverFromHost is the explicit low-level exception: when the host supplies
// River, the host owns that River client's schema and its migrations.
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool, schema string) error {
	if pool == nil {
		return fmt.Errorf("openrails embed: postgres pool is required")
	}
	var err error
	if schema, err = validateMigrationSchema(schema); err != nil {
		return err
	}
	return migrate.ApplyPostgresMigrations(ctx, pool, schema)
}

func validateMigrationSchema(schema string) (string, error) {
	schema = strings.TrimSpace(schema)
	if schema == "" {
		schema = config.DefaultSchema
	}
	if len(schema) > 63 || !migrationSchemaName.MatchString(schema) {
		return "", fmt.Errorf("openrails embed: invalid database schema %q", schema)
	}
	return schema, nil
}
