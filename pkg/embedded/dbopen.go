package embedded

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// openEmbeddedDB is the ONE place a host's database handle enters a
// manifest-plane entry point (catalog push/dump, converge, prune/rollback,
// admin grants, billing import, provider pull). It wraps a caller-supplied pool
// (borrowed; Close is a no-op) or opens one from config, and then enforces the
// SAME RLS-posture gate `embedded.New` runs at boot (or#885).
//
// Without this, the gate covered only the long-lived runtime: a host could be
// refused at `embedded.New` for connecting privileged and still run a catalog
// push or a billing import on that very pool, where RLS is inert and a query
// that forgets its merchant scope reads other merchants' rows instead of the
// empty result the policy would return. The posture is a property of the
// connected ROLE, so it must be checked wherever a connection is accepted —
// not once per process.
//
// Callers keep their existing `defer database.Close()`; Close is a no-op for a
// borrowed pool.
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
		database, err = db.NewDB(cfg.DB)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
	}
	if err := database.EnforceRLSPosture(ctx); err != nil {
		_ = database.Close()
		return nil, err
	}
	return database, nil
}
