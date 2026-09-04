package riverjobs

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
)

const KindMerchantSecretCleanup = "openrails.merchant_secret_cleanup"

type MerchantSecretCleanupArgs struct{}

func (MerchantSecretCleanupArgs) Kind() string { return KindMerchantSecretCleanup }

// MerchantSecretCleanupWorker resumes external cleanup of already committed
// purges. It cannot initiate a purge or select an active merchant.
type MerchantSecretCleanupWorker struct {
	river.WorkerDefaults[MerchantSecretCleanupArgs]
	DB        *db.DB
	Merchants *merchants.Service
}

func (MerchantSecretCleanupWorker) Kind() string { return KindMerchantSecretCleanup }
func (w MerchantSecretCleanupWorker) Work(ctx context.Context, _ *river.Job[MerchantSecretCleanupArgs]) error {
	if w.DB == nil || w.Merchants == nil {
		return fmt.Errorf("merchant secret cleanup is not configured")
	}
	var after *uuid.UUID
	var firstErr error
	for {
		rows, err := w.DB.GenDirectory().ListPendingMerchantSecretCleanups(ctx, gen.ListPendingMerchantSecretCleanupsParams{AfterRunID: after, PageLimit: 100})
		if err != nil {
			return fmt.Errorf("list pending merchant secret cleanup: %w", err)
		}
		for _, row := range rows {
			if row.RunID == nil || row.MerchantID == nil {
				return fmt.Errorf("merchant secret cleanup work list contains an invalid identity")
			}
			after = row.RunID
			progress.Mark(ctx, "merchant secret cleanup "+row.RunID.String())
			if err := w.Merchants.RetrySecretCleanup(ctx, merchant.ID(*row.MerchantID), *row.RunID); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if len(rows) < 100 {
			return firstErr
		}
	}
}
