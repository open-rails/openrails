package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/money/books"
)

const (
	KindRevenueRecognition = "openrails.revenue_recognition"

	// revenueRecognitionBatch is how many due schedules one read takes. The
	// read is indexed on the due instant, so a pass costs what is due: at a
	// month's end, the live schedules; otherwise the ones ending.
	revenueRecognitionBatch = 200
)

// RevenueRecognitionWorker recognises every revenue schedule due: the month
// boundaries reached in each merchant's accounting timezone, schedules that
// ended and early ends. It runs from due instants alone, never over every
// schedule; an idle pass reads no row. A failed schedule stays due and fails
// the pass once the others are done.
type RevenueRecognitionWorker struct {
	DB    *db.DB
	Clock clockwork.Clock
	Log   *slog.Logger
}

// DueBy reports a schedule due since before at.
func (w RevenueRecognitionWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	return w.DB.GenDirectory().RevenueRecognitionDueBefore(ctx, at)
}

func (w RevenueRecognitionWorker) Pass(ctx context.Context) error {
	now := workerNow(w.Clock)
	logger := w.Log.With("worker", KindRevenueRecognition)
	args := gen.ListDueRevenueSchedulesParams{Now: now, RowLimit: revenueRecognitionBatch}
	zones := map[uuid.UUID]*time.Location{}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueRevenueSchedules(ctx, args)
		if err != nil {
			return errors.Join(failed, err)
		}
		for _, row := range due {
			if err := ctx.Err(); err != nil {
				return errors.Join(failed, fmt.Errorf("revenue recognition: %w", err))
			}
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "revenue recognition", func(ctx context.Context) error {
				loc, ok := zones[row.MerchantID]
				if !ok {
					var err error
					if loc, err = books.AccountingZone(ctx, w.DB); err != nil {
						return err
					}
					zones[row.MerchantID] = loc
				}
				return books.Recognize(ctx, w.DB, row.ID, loc, now)
			})
			if err != nil {
				// One schedule's failure must not hold up the rest: it stays due.
				logger.ErrorContext(ctx, "revenue recognition: schedule failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "revenue_schedule_id", row.ID)
				failed = errors.Join(failed, fmt.Errorf("revenue recognition: merchant %s schedule %s: %w", row.MerchantID, row.ID, err))
			}
		}
		if len(due) < revenueRecognitionBatch {
			return failed
		}
		last := due[len(due)-1]
		args.AfterAt, args.AfterMerchantID, args.AfterID = &last.NextRecognitionAt, &last.MerchantID, &last.ID
	}
}
