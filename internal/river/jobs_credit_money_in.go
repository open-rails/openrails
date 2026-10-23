package riverjobs

import (
	"context"
	"encoding/json"
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
	Automatic             bool                `json:"automatic,omitempty"`
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
	var metadata struct {
		Periodic bool `json:"periodic"`
	}
	if job.JobRow != nil {
		_ = json.Unmarshal(job.Metadata, &metadata)
	}
	if metadata.Periodic {
		// Old releases scheduled phase-specific jobs. Their metadata identifies
		// them as automatic work; they never bypass the host opt-in.
		copy := *job
		copy.Args = UsageOrderArgs{MerchantID: job.Args.MerchantID, Automatic: true}
		job = &copy
	}
	manual := !job.Args.Automatic
	if !manual && (w.Config == nil || !w.Config.UsageBillingEnabled) {
		return nil
	}
	if manual && job.Args.Collect && config.IsLimitedMode(w.Config) {
		if job.Args.MerchantID == nil && !job.Args.FinalizePreviousMonth {
			if job.Args.UseMonthlyFloor {
				return river.JobSnooze(time.Hour)
			}
			return nil
		}
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
	// Explicit phase flags remain the host's manual billing API. Automatic
	// work uses no phase flags and always follows the account due records.
	if !job.Args.Automatic {
		settings, err := w.Money.UsageSettings(ctx)
		if err != nil {
			return err
		}
		if _, err = w.Money.FinalizeThresholdUsage(ctx, now, money.UsageThresholdOptions{CollectionThresholdAmount: settings.CollectionThresholdAmount, BillingPeriodBoundary: settings.BillingPeriodBoundary}); err != nil {
			return err
		}
		if job.Args.FinalizePreviousMonth {
			if _, err = w.Money.FinalizeDueUsageForBoundary(ctx, settings.BillingPeriodBoundary, now); err != nil {
				return err
			}
		}
		if !job.Args.Collect || w.Intents == nil {
			return nil
		}
		minimum := settings.CollectionThresholdAmount
		if job.Args.UseMonthlyFloor {
			minimum = settings.MonthlyFloorAmount
		}
		_, err = w.Money.ChargeOutstanding(ctx, w.Intents, minimum)
		return err
	}
	finalized, finalizeErr := w.Money.FinalizeDueUsage(ctx, now)
	var failures []error
	if finalizeErr != nil {
		failures = append(failures, finalizeErr)
	}
	if finalized > 0 {
		logger.WithField("orders", finalized).Info("usage orders billed")
	}
	if ctx.Err() != nil {
		return errors.Join(append(failures, ctx.Err())...)
	}
	if _, err := w.Money.NotifyOverdueUsageOrders(ctx, now); err != nil {
		failures = append(failures, err)
	}
	if w.Intents == nil || config.IsLimitedMode(w.Config) {
		return errors.Join(failures...)
	}
	if _, err := w.Money.ChargeOutstanding(ctx, w.Intents, 0); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
