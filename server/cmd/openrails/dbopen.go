package main

import (
	"context"
	"fmt"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
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

// bindCLIRiverProducer binds an insert-only River client in the configured
// River schema: jobs a command commits wait there for the fleet.
func bindCLIRiverProducer(ctx context.Context, cfg *config.Config, database *db.DB) error {
	schema := config.RiverSchemaName(cfg)
	producer, err := river.NewClient(riverpgxv5.New(database.Pool()), &river.Config{Schema: schema, SkipUnknownJobCheck: true})
	if err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	if err := database.ValidateRiverJobBinding(ctx, database.Pool(), schema); err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	database.SetRiverJobInserter(producer)
	return nil
}
