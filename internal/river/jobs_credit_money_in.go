package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

// forEachActiveMerchant runs fn once per active merchant inside its
// RunInMerchantConn: River job contexts carry no merchant, and every money
// path these workers call requires one. One merchant's failure does not abort
// the rest; the joined error makes a failing run visible in River.
func forEachActiveMerchant(ctx context.Context, dbi *db.DB, logger *log.Entry, fn func(ctx context.Context) error) error {
	if dbi == nil {
		logger.Debug("db not configured; skipping")
		return nil
	}
	merchantIDs, err := dbi.GenDirectory().ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list merchants: %w", err)
	}
	var errs []error
	for _, mid := range merchantIDs {
		progress.Mark(ctx, "credit money-in merchant "+mid.String())
		mctx := merchant.WithID(ctx, billing.MerchantID(mid))
		if err := dbi.RunInMerchantConn(mctx, fn); err != nil {
			logger.WithError(err).WithField("merchant_id", mid).Error("merchant failed; continuing")
			errs = append(errs, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	return errors.Join(errs...)
}

// --- Usage orders and arrears collection ---

// KindUsageOrders is the job's kind as queued jobs carry it: earlier
// releases named the job for invoices.
const KindUsageOrders = "openrails.invoice"

// UsageOrderArgs selects the phases of one usage-order pass: bill periods,
// notify overdue orders and collect them.
type UsageOrderArgs struct {
	MerchantID            *billing.MerchantID `json:"merchant_id,omitempty"`
	Collect               bool                `json:"collect,omitempty"`
	UseMonthlyFloor       bool                `json:"use_monthly_floor,omitempty"`
	FinalizePreviousMonth bool                `json:"finalize_previous_month,omitempty"`
}

func (UsageOrderArgs) Kind() string { return KindUsageOrders }

type UsageOrderWorker struct {
	river.WorkerDefaults[UsageOrderArgs]
	DB    *db.DB
	Money *money.MoneyService
	// Intents runs the order_collection operations; nil skips collection.
	// Unknown outcomes are the scheduled intent verifier's, not this worker's.
	Intents *intents.Runner
	Config  *config.Config
	Clock   clockwork.Clock
}

func (UsageOrderWorker) Kind() string { return KindUsageOrders }

func (w UsageOrderWorker) Work(ctx context.Context, job *river.Job[UsageOrderArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindUsageOrders)
	if w.Money == nil {
		logger.Debug("money service not configured; skipping usage orders")
		return nil
	}
	if job.Args.Collect && config.IsLimitedMode(w.Config) {
		// Keep the durable period slot pending. Completing a disabled monthly
		// scan would suppress collection for the rest of its thirty-day bucket,
		// even after a restart with writes enabled.
		return river.JobSnooze(time.Minute)
	}
	if job.Args.MerchantID != nil {
		if job.Args.MerchantID.IsZero() {
			return errors.New("a usage-order pass requires a nonzero merchant ID")
		}
		ctx = merchant.WithID(ctx, *job.Args.MerchantID)
		return w.DB.RunInMerchantConn(ctx, func(ctx context.Context) error { return w.workMerchant(ctx, job, logger) })
	}
	// Every money path below (settings, finalize, collect) requires a merchant
	// in context: fan out per merchant.
	return forEachActiveMerchant(ctx, w.DB, logger, func(ctx context.Context) error {
		return w.workMerchant(ctx, job, logger)
	})
}

// workMerchant runs one merchant's pass on a merchant-scoped context.
func (w UsageOrderWorker) workMerchant(ctx context.Context, job *river.Job[UsageOrderArgs], logger *log.Entry) error {
	now := time.Now().UTC()
	if w.Clock != nil {
		now = w.Clock.Now().UTC()
	}
	settings, err := w.Money.UsageSettings(ctx)
	if err != nil {
		return err
	}

	finalized, err := w.Money.FinalizeThresholdUsage(ctx, now, money.UsageThresholdOptions{
		CollectionThresholdAmount: settings.CollectionThresholdAmount,
		BillingPeriodBoundary:     settings.BillingPeriodBoundary,
	})
	if err != nil {
		return err
	}
	if finalized > 0 {
		logger.WithField("orders", finalized).Info("threshold usage orders billed")
	}

	if job.Args.FinalizePreviousMonth {
		n, err := w.Money.FinalizeDueUsageForBoundary(ctx, settings.BillingPeriodBoundary, now)
		if err != nil {
			return err
		}
		if n > 0 {
			logger.WithField("orders", n).Info("monthly usage orders billed")
		}
	}

	if job.Args.Collect {
		// Customers hear about overdue net-N orders before collection,
		// even when no charger is armed.
		if n, err := w.Money.NotifyOverdueUsageOrders(ctx, now); err != nil {
			return err
		} else if n > 0 {
			logger.WithField("orders", n).Info("overdue order notices queued")
		}
		if w.Intents == nil {
			logger.Debug("order collection runner not configured; skipping collection")
			return nil
		}
		threshold, collect := settings.CollectionThresholdAmount, w.Money.ChargeOutstanding
		if job.Args.UseMonthlyFloor {
			threshold, collect = settings.MonthlyFloorAmount, w.Money.ChargeMonthlyOutstanding
		}
		n, err := collect(ctx, w.Intents, threshold)
		if err != nil {
			return err
		}
		if n > 0 {
			logger.WithField("charged", n).Info("usage order collection completed")
		}
	}
	return nil
}
