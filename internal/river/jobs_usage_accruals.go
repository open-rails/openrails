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
	KindUsageAccruals = "openrails.usage_accruals"

	// usageAccrualBatch is how many due accruals one read takes. The read is
	// indexed on the accrual's day, so a pass costs what is due, never what
	// accrued.
	usageAccrualBatch = 200
)

// UsageAccrualWorker posts the usage accruals whose UTC day ended: one
// deduction per customer, currency and day, on the money service's clock. An
// idle pass reads no row. A customer whose posting failed stays due and fails
// the pass once the others are done.
type UsageAccrualWorker struct {
	DB    *db.DB
	Money *money.MoneyService
	Log   *slog.Logger
}

// DueBy reports an accrual of a day before at's still unposted.
func (w UsageAccrualWorker) DueBy(ctx context.Context, at time.Time) (bool, error) {
	due, err := w.DB.GenDirectory().ListDueUsageAccruals(ctx, gen.ListDueUsageAccrualsParams{DayAt: at.UTC().Truncate(24 * time.Hour), RowLimit: 1})
	return len(due) > 0, err
}

func (w UsageAccrualWorker) Pass(ctx context.Context) error {
	now := w.Money.Clock().Now().UTC()
	today := now.Truncate(24 * time.Hour)
	logger := w.Log.With("worker", KindUsageAccruals)
	args := gen.ListDueUsageAccrualsParams{DayAt: today, RowLimit: usageAccrualBatch}
	var failed error
	for {
		due, err := w.DB.GenDirectory().ListDueUsageAccruals(ctx, args)
		if err != nil {
			return errors.Join(failed, err)
		}
		for _, row := range due {
			err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "usage accruals", func(ctx context.Context) error {
				_, err := w.Money.PostUsageAccruals(ctx, row.CustomerID, row.Currency, today, now)
				return err
			})
			if err != nil {
				logger.ErrorContext(ctx, "usage accruals: posting failed; continuing", logx.Err(err), "merchant_id", row.MerchantID, "customer_id", row.CustomerID, "currency", row.Currency)
				failed = errors.Join(failed, fmt.Errorf("usage accruals: merchant %s customer %s %s: %w", row.MerchantID, row.CustomerID, row.Currency, err))
			}
			if ctx.Err() != nil {
				return errors.Join(failed, ctx.Err())
			}
		}
		if len(due) < usageAccrualBatch {
			return failed
		}
		last := due[len(due)-1]
		args.AfterDayAt, args.AfterMerchantID, args.AfterCustomerID, args.AfterCurrency = &last.DayAt, &last.MerchantID, &last.CustomerID, &last.Currency
	}
}
