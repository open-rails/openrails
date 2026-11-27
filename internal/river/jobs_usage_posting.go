package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/money"
)

const (
	KindUsagePosting = "openrails.usage_posting"

	// usagePostingBatch is how many due customers one read takes. The read is
	// indexed on due_at, so a pass costs what is due, never what has usage.
	usagePostingBatch = 200
)

// UsagePostingWorker is the daily posting: each customer whose pending usage
// is due (the accounting-zone day of its oldest usage ended) posts it, on the
// money service's clock. An idle pass reads no row. A
// customer whose posting failed stays due and fails the pass once the others
// are done.
type UsagePostingWorker struct {
	DB    *db.DB
	Money *money.MoneyService
	Log   *slog.Logger
}

// DueBy reports a customer whose usage was due to post by at.
func (w UsagePostingWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	due, err := w.DB.GenDirectory().ListDueUsagePending(ctx, gen.ListDueUsagePendingParams{DueAt: at.UTC(), RowLimit: 1})
	return len(due) > 0, err
}

func (w UsagePostingWorker) Pass(ctx context.Context) error {
	now := w.Money.Clock().Now().UTC()
	logger := w.Log.With("worker", KindUsagePosting)
	args := gen.ListDueUsagePendingParams{DueAt: now, RowLimit: usagePostingBatch}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueUsagePending(ctx, args)
		if err != nil {
			return errors.Join(failed, err)
		}
		for _, row := range due {
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "usage posting", func(ctx context.Context) error {
				_, err := w.Money.PostDueUsage(ctx, row.CustomerID, row.Currency, now)
				return err
			})
			if err != nil {
				logger.ErrorContext(ctx, "usage posting failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "customer_id", row.CustomerID, "currency", row.Currency)
				failed = errors.Join(failed, fmt.Errorf("usage posting: merchant %s customer %s %s: %w", row.MerchantID, row.CustomerID, row.Currency, err))
			}
			if ctx.Err() != nil {
				return errors.Join(failed, ctx.Err())
			}
		}
		if len(due) < usagePostingBatch {
			return failed
		}
		last := due[len(due)-1]
		args.AfterDueAt, args.AfterMerchantID, args.AfterCustomerID, args.AfterCurrency = &last.DueAt, &last.MerchantID, &last.CustomerID, &last.Currency
	}
}
