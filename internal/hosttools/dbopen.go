package hosttools

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// openEmbeddedDB borrows the host pool or opens the configured database.
// Callers provide explicit merchant scope; Close leaves borrowed pools open.
func openEmbeddedDB(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool) (*db.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var (
		database *db.DB
		err      error
	)
	if pool != nil {
		schema := config.DefaultSchema
		if cfg != nil && cfg.DB != nil {
			schema = cfg.DB.SchemaName()
		}
		database, err = db.NewWithPGXPool(pool, schema)
		if err != nil {
			return nil, err
		}
	} else {
		if cfg == nil || cfg.DB == nil {
			return nil, fmt.Errorf("config database is required")
		}
		database, err = db.NewDB(ctx, cfg.DB)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
	}
	return database, nil
}
