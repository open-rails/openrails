package riverjobs

import (
	"context"

	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const KindPriceMigrationRedrive = "openrails.price_migration_redrive"

type PriceMigrationRedriveArgs struct{}

func (PriceMigrationRedriveArgs) Kind() string { return KindPriceMigrationRedrive }

// PriceMigrationRedriveWorker retries price migrations' failed provider
// pushes: a deferred push once the subscription is in its last period before
// the effective date, and a push that landed before its database write.
// Hourly suffices: periods are days long, and every transition is
// status-predicated and every push idempotent.
type PriceMigrationRedriveWorker struct {
	river.WorkerDefaults[PriceMigrationRedriveArgs]
	Migrations *subscriptions.PriceMigrationService
	BatchSize  int
}

func (PriceMigrationRedriveWorker) Kind() string { return KindPriceMigrationRedrive }

func (w PriceMigrationRedriveWorker) Work(ctx context.Context, _ *river.Job[PriceMigrationRedriveArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindPriceMigrationRedrive)
	if w.Migrations == nil {
		logger.Debug("price migration service not wired; skipping")
		return nil
	}
	res, err := w.Migrations.RedriveBlocked(ctx, w.BatchSize)
	if err != nil {
		return err
	}
	if res.Examined > 0 {
		logger.WithFields(log.Fields{
			"examined": res.Examined, "redriven": res.Redriven, "converged": res.Converged,
			"deferred": res.Deferred, "skipped": res.Skipped, "failed": res.Failed,
		}).Info("price-migration re-drive pass complete")
	}
	return nil
}
