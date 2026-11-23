package guide

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails"
)

// EmbeddedFleet runs OpenRails' jobs on the host's own River fleet.
func EmbeddedFleet(ctx context.Context, pool *pgxpool.Pool, bill *openrails.Engine) error {
	// guide:embedded-fleet
	fleet, err := riverhelpers.New(ctx, pool, &river.Config{
		Schema: "billing_river", // Config.Database.RiverSchema
		Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 10}},
	}, bill.RiverJobs()) // and any other library's jobs, e.g. AuthKit's
	if err != nil {
		return err
	}
	if err := fleet.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = fleet.StopAndCancel(context.WithoutCancel(ctx)) }()
	if err := bill.Start(ctx, openrails.WithRiverClient(fleet)); err != nil {
		return err
	}
	// guide:end
	<-ctx.Done()
	return nil
}
