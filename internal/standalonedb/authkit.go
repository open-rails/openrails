// Package standalonedb provisions the standalone product's optional components.
// It is not part of the provider-neutral billing migration path.
package standalonedb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/embedded"
)

// ApplyAuthKit initializes standalone identity and optional direct runtime access.
// The owner is privileged; the host supplies the runtime login and credentials.
func ApplyAuthKit(ctx context.Context, owner, runtime *pgxpool.Pool) error {
	opts := authcore.MigrationOptions{River: authcore.RiverFromHost(), RuntimePool: runtime}
	if err := authcore.ApplyMigrations(ctx, owner, "profiles", opts); err != nil {
		return fmt.Errorf("standalone AuthKit migrations: %w", err)
	}
	return nil
}
