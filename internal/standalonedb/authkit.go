// Package standalonedb provisions the standalone product's optional components.
// It is not part of the provider-neutral billing migration path.
package standalonedb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
)

// ApplyAuthKit migrates the control plane's AuthKit schema (the default,
// profiles) as the role the server runs as, and River's tables in
// riverSchema, the fleet AuthKit's jobs run on (idempotent beside
// OpenRails' own River migrations).
func ApplyAuthKit(ctx context.Context, pool *pgxpool.Pool, riverSchema string) error {
	cfg := authkit.Config{RiverSchema: riverSchema}
	if err := authkit.Migrate(ctx, pool, cfg, authkit.MigrateOptions{}); err != nil {
		return fmt.Errorf("standalone AuthKit migrations: %w", err)
	}
	return nil
}
