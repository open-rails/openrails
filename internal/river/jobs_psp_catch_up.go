package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile"
)

// staleRetryAfter is how long a stale PSP rests after a failed read before
// its catch-up tries again.
const staleRetryAfter = time.Hour

// PSPCatchUpWorker looks at one PSP: records why it cannot be read, or reads
// a stale one's gap before detection resumes (reconcile.Verifier.CatchUp).
type PSPCatchUpWorker struct {
	WorkerDefaults[reconcile.CatchUpArgs]
	Verifier *reconcile.Verifier
}

func (PSPCatchUpWorker) Kind() string { return reconcile.KindPSPCatchUp }

func (w *PSPCatchUpWorker) Work(ctx context.Context, job *river.Job[reconcile.CatchUpArgs]) error {
	if w.Verifier == nil {
		return river.JobCancel(errors.New("psp catch-up: verifier not wired"))
	}
	if job.Args.MerchantID == uuid.Nil || job.Args.PSPID == uuid.Nil {
		return river.JobCancel(errors.New("psp catch-up names no merchant and PSP"))
	}
	return w.Verifier.CatchUp(ctx, billing.MerchantID(job.Args.MerchantID), job.Args.PSPID)
}

// queueCatchUps queues the catch-up of each stale PSP that has not failed a
// read within staleRetryAfter. Work grows with the stale PSPs, not the
// subscriptions.
func queueCatchUps(ctx context.Context, database *db.DB, now time.Time) error {
	afterMerchant, afterPSP := uuid.Nil, uuid.Nil
	for {
		page, err := database.GenDirectory().ListStalePSPs(ctx, gen.ListStalePSPsParams{
			RetryBefore: now.Add(-staleRetryAfter), AfterMerchantID: afterMerchant, AfterPspID: afterPSP, RowLimit: dunningMerchantBatch,
		})
		if err != nil {
			return fmt.Errorf("catch-ups: list stale PSPs: %w", err)
		}
		if len(page) > 0 {
			if err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
				for _, row := range page {
					if _, err := database.InsertRiverJobTx(ctx, tx, reconcile.CatchUpArgs{MerchantID: row.MerchantID, PSPID: row.PspID}, nil); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return fmt.Errorf("catch-ups: queue: %w", err)
			}
		}
		if len(page) < dunningMerchantBatch {
			return nil
		}
		last := page[len(page)-1]
		afterMerchant, afterPSP = last.MerchantID, last.PspID
	}
}
