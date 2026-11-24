package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
)

func withCLIDB(cmd *cobra.Command, run func(context.Context, *db.DB) error) error {
	ctx := cmd.Context()
	database, err := openCLIDB(ctx)
	if err != nil {
		return err
	}
	defer database.Close()
	return run(ctx, database)
}

// bindCLIRiverProducer binds an insert-only River client in the configured
// River schema: jobs a command commits wait there for the fleet.
func bindCLIRiverProducer(ctx context.Context, cfg *billing.Config, database *db.DB) error {
	schema := config.RiverSchemaName(cfg)
	producer, err := river.NewClient(riverpgxv5.New(database.Pool()), &river.Config{Schema: schema, SkipUnknownJobCheck: true, Logger: slog.Default()})
	if err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	if err := database.ValidateRiverJobBinding(ctx, database.Pool(), schema); err != nil {
		return fmt.Errorf("River producer: %w", err)
	}
	database.SetRiverJobInserter(producer)
	return nil
}
