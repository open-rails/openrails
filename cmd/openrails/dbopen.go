package main

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// openCLIDB opens the configured database. Tenant operations carry their
// authorized merchant explicitly; PostgreSQL role flags do not select a tenant.
func openCLIDB(ctx context.Context, cfg *config.Config) (*db.DB, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg == nil || cfg.DB == nil {
		return nil, fmt.Errorf("config not loaded")
	}
	database, err := db.NewDB(ctx, cfg.DB)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return database, nil
}
