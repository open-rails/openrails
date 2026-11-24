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
	KindUsageClose = "openrails.usage_close"

	// usageCloseBatch is how many due customers one read takes. The read is
	// indexed on due_at, so a pass costs what is due, never what has usage.
	usageCloseBatch = 200
)

// UsageCloseWorker is the month-end close: each customer whose unposted
// usage is due (its month ended, or a period whose price fell may be
// credited) posts it, on the money service's clock. An idle pass reads no
// row. A customer whose posting failed stays due and fails the pass once the
// others are done.
type UsageCloseWorker struct {
	DB    *db.DB
	Money *money.MoneyService
	Log   *slog.Logger
}

// DueBy reports a customer whose usage was due to post by at.
func (w UsageCloseWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	due, err := w.DB.GenDirectory().ListDueUsageUnposted(ctx, gen.ListDueUsageUnpostedParams{DueAt: at.UTC(), RowLimit: 1})
	return len(due) > 0, err
}

func (w UsageCloseWorker) Pass(ctx context.Context) error {
	now := w.Money.Clock().Now().UTC()
	logger := w.Log.With("worker", KindUsageClose)
	args := gen.ListDueUsageUnpostedParams{DueAt: now, RowLimit: usageCloseBatch}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueUsageUnposted(ctx, args)
		if err != nil {
			return errors.Join(failed, err)
		}
		for _, row := range due {
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "usage close", func(ctx context.Context) error {
				_, err := w.Money.CloseUsage(ctx, row.CustomerID, row.Currency, now)
				return err
			})
			if err != nil {
				logger.ErrorContext(ctx, "usage close: posting failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "customer_id", row.CustomerID, "currency", row.Currency)
				failed = errors.Join(failed, fmt.Errorf("usage close: merchant %s customer %s %s: %w", row.MerchantID, row.CustomerID, row.Currency, err))
			}
			if ctx.Err() != nil {
				return errors.Join(failed, ctx.Err())
			}
		}
		if len(due) < usageCloseBatch {
			return failed
		}
		last := due[len(due)-1]
		args.AfterDueAt, args.AfterMerchantID, args.AfterCustomerID, args.AfterCurrency = &last.DueAt, &last.MerchantID, &last.CustomerID, &last.Currency
	}
}
