package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/internal/config"
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
	connect, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	database, err := db.NewDBOnce(connect, cfg)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	return database, nil
}

func withCLIDB(cmd *cobra.Command, run func(context.Context, *db.DB) error) error {
	ctx := cmd.Context()
	cfg, _ := ctx.Value(config.ConfigContextKey).(*config.Config)
	database, err := openCLIDB(ctx, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
	return run(ctx, database)
}
