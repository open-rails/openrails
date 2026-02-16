package riverjobs

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/services"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	KindIdempotencyCleanup = "billing.idempotency_cleanup"
	KindCCBillReconcile    = "billing.ccbill_reconcile"
)

type IdempotencyCleanupArgs struct{}

func (IdempotencyCleanupArgs) Kind() string { return KindIdempotencyCleanup }

type IdempotencyCleanupWorker struct {
	river.WorkerDefaults[IdempotencyCleanupArgs]
	DB     *db.DB
	Config *config.Config
}

func (IdempotencyCleanupWorker) Kind() string { return KindIdempotencyCleanup }

func (w IdempotencyCleanupWorker) Work(ctx context.Context, job *river.Job[IdempotencyCleanupArgs]) error {
	if w.DB == nil {
		return fmt.Errorf("db is required")
	}

	retentionDays := 90
	if w.Config != nil {
		retentionDays = w.Config.GetWebhookDedupeRetentionDays()
	}

	dedupeService := services.NewDeduplicationService(nil, w.DB)
	deletedRows, err := dedupeService.CleanupOldWebhooks(ctx, retentionDays)
	if err != nil {
		return fmt.Errorf("cleanup durable webhook dedupe records: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"retention_days": retentionDays,
		"deleted_rows":   deletedRows,
	}).Info("IdempotencyCleanup: completed")
	return nil
}

type CCBillReconcileArgs struct{}

func (CCBillReconcileArgs) Kind() string { return KindCCBillReconcile }

type CCBillReconcileWorker struct {
	river.WorkerDefaults[CCBillReconcileArgs]
	DB       *db.DB
	DataLink *ccbill.DataLinkClient
}

func (CCBillReconcileWorker) Kind() string { return KindCCBillReconcile }

func (w CCBillReconcileWorker) Work(ctx context.Context, job *river.Job[CCBillReconcileArgs]) error {
	if w.DataLink == nil {
		log.WithContext(ctx).Info("CCBillReconcile: DataLink not configured; skipping")
		return nil
	}
	records, err := w.DataLink.FetchActiveMembers(ctx)
	if err != nil {
		return fmt.Errorf("ccbill datalink reconcile: %w", err)
	}
	log.WithContext(ctx).WithField("record_count", len(records)).Info("CCBillReconcile: fetched active members")
	// TODO: persist or compare with internal state
	return nil
}
