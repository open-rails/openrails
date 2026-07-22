package riverjobs

import (
	"context"

	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const KindPlanMigrationRedrive = "openrails.plan_migration_redrive"

type PlanMigrationRedriveArgs struct{}

func (PlanMigrationRedriveArgs) Kind() string { return KindPlanMigrationRedrive }

// PlanMigrationRedriveWorker (#816) is the background half of #813 plan
// migrations: it re-drives blocked plan-change rows through the SAME
// classify/execute paths a manual re-run uses — deferred far-future pushes as
// each subscription enters its final pre-effective period, and crash-window
// rows whose rail already carries the target. Hourly is plenty: period
// granularity is days, and every transition is status-predicated + rail-
// idempotent, so an extra tick (or a concurrent manual re-run) can never
// double-push.
type PlanMigrationRedriveWorker struct {
	river.WorkerDefaults[PlanMigrationRedriveArgs]
	Migrations *subscriptions.PlanMigrationService
	BatchSize  int
}

func (PlanMigrationRedriveWorker) Kind() string { return KindPlanMigrationRedrive }

func (w PlanMigrationRedriveWorker) Work(ctx context.Context, _ *river.Job[PlanMigrationRedriveArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindPlanMigrationRedrive)
	if w.Migrations == nil {
		// Worker-only runtimes without the migration service wired: log-and-skip,
		// same posture as the money-in workers awaiting their charger.
		logger.Debug("plan migration service not wired; skipping")
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
		}).Info("plan-migration re-drive pass complete")
	}
	return nil
}
