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
		mctx := merchant.WithID(ctx, billing.MerchantID(mid))
		if err := dbi.RunInMerchantConn(mctx, fn); err != nil {
			logger.WithError(err).WithField("merchant_id", mid).Error("merchant failed; continuing")
			errs = append(errs, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	return errors.Join(errs...)
}

// UsageOrderWorker bills the usage orders that are due and collects them once
// an off-session charger is configured. A pass with a merchant is that merchant's threshold wakeup.
type UsageOrderWorker struct {
	WorkerDefaults[money.UsageOrderArgs]
	DB    *db.DB
	Money *money.MoneyService
	// Intents runs the order_collection operations; nil skips collection.
	// Unknown outcomes are the scheduled intent verifier's, not this worker's.
	Intents *intents.Runner
	Config  *config.Config
	Clock   clockwork.Clock
}

func (UsageOrderWorker) Kind() string { return money.KindUsageOrders }

func (w UsageOrderWorker) Work(ctx context.Context, job *river.Job[money.UsageOrderArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", money.KindUsageOrders)
	if w.Money == nil || w.Config == nil || !w.Config.UsageBillingEnabled {
		return nil
	}
	if job.Args.MerchantID != nil {
		if job.Args.MerchantID.IsZero() {
			return errors.New("a usage-order pass requires a nonzero merchant ID")
		}
		ctx = merchant.WithID(ctx, *job.Args.MerchantID)
		return w.DB.RunInMerchantConn(ctx, func(ctx context.Context) error { return w.workMerchant(ctx, logger, job.Args.After) })
	}
	// Every money path below (finalize, notify, collect) requires a merchant
	// in context: fan out per merchant.
	return forEachActiveMerchant(ctx, w.DB, logger, func(ctx context.Context) error {
		return w.workMerchant(ctx, logger, nil)
	})
}

// workMerchant runs one merchant's pass on a merchant-scoped context: a page
// of due accounts after after. A full page continues in a new job, which
// notifies and collects once the merchant's due accounts are billed.
func (w UsageOrderWorker) workMerchant(ctx context.Context, logger *log.Entry, after *money.UsageAccount) error {
	now := time.Now().UTC()
	if w.Clock != nil {
		now = w.Clock.Now().UTC()
	}
	finalized, next, finalizeErr := w.Money.FinalizeDueUsage(ctx, now, after)
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
	if next != nil {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		return errors.Join(append(failures, continuePass(ctx, money.UsageOrderArgs{MerchantID: &mid, After: next}))...)
	}
	if w.Intents == nil || config.IsLimitedMode(w.Config) {
		return errors.Join(failures...)
	}
	if _, err := w.Money.ChargeOutstanding(ctx, w.Intents); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}
