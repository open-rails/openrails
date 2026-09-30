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
// profiles) through the privileged owner pool and grants the runtime login
// access. OpenRails owns the River fleet, so River's tables are not AuthKit's.
func ApplyAuthKit(ctx context.Context, owner, runtime *pgxpool.Pool) error {
	cfg := authkit.Config{River: authkit.RiverConfig{HostOwned: true}}
	if err := authkit.Migrate(ctx, owner, cfg, authkit.MigrateOptions{RuntimePool: runtime}); err != nil {
		return fmt.Errorf("standalone AuthKit migrations: %w", err)
	}
	return nil
}
