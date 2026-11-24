package riverjobs

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// PriceMigrationWorker reaches one page of a price migration's subscribers,
// completing its job in the page's transaction. A page that fails its last
// attempt ends the walk, so the migration is not left preparing.
type PriceMigrationWorker struct {
	WorkerDefaults[subscriptions.PriceMigrationArgs]
	Migrations *subscriptions.PriceMigrationService
}

func (w *PriceMigrationWorker) Work(ctx context.Context, job *river.Job[subscriptions.PriceMigrationArgs]) error {
	if w.Migrations == nil {
		return errors.New("price migration worker is not wired")
	}
	err := w.Migrations.RunPage(ctx, job.Args, func(ctx context.Context, tx pgx.Tx) error {
		_, err := river.JobCompleteTx[*riverpgxv5.Driver](ctx, db.RiverTx(tx), job)
		return err
	})
	if err != nil && job.Attempt >= job.MaxAttempts {
		return errors.Join(err, w.Migrations.StopWalk(ctx, job.Args, err))
	}
	return err
}
