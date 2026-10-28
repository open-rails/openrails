package riverjobs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// continuePass queues the next job of a pass that stopped at its bound, so
// each job stays short and a crash loses only the job in flight.
func continuePass(ctx context.Context, args river.JobArgs) error {
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("continue %s: %w", args.Kind(), err)
	}
	if _, err := client.Insert(ctx, args, nil); err != nil {
		return fmt.Errorf("continue %s: %w", args.Kind(), err)
	}
	return nil
}
