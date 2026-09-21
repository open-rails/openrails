// Package standalonedb provisions the standalone product's optional components.
// It is not part of the provider-neutral billing migration path.
package standalonedb

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	authcore "github.com/open-rails/authkit/embedded"
)

// ApplyAuthKit initializes the standalone identity service using AuthKit's own
// migration API, then grants the standalone runtime role data access. The pool
// must belong to the privileged migration owner; runtime startup never calls
// this function. Apply the billing baseline first to provision openrails_app.
func ApplyAuthKit(ctx context.Context, owner *pgxpool.Pool) error {
	if err := authcore.ApplyMigrations(ctx, owner, "profiles", authcore.MigrationOptions{River: authcore.RiverFromHost()}); err != nil {
		return fmt.Errorf("standalone AuthKit migrations: %w", err)
	}
	// These are standalone deployment privileges, not billing's schema contract.
	// Reapplying provisioning grants access to new AuthKit tables as it evolves.
	_, err := owner.Exec(ctx, `
 GRANT USAGE ON SCHEMA profiles TO openrails_app;
 GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA profiles TO openrails_app;
 GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA profiles TO openrails_app;
 `)
	if err != nil {
		return fmt.Errorf("standalone AuthKit runtime grants: %w", err)
	}
	return nil
}
