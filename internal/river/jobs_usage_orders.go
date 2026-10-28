package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money"
)

// usagePage bounds one job's due accounts, and its due collections; a full
// page continues in a new job.
const usagePage = 200

// UsageOrderWorker is the usage pass across merchants: it bills the usage
// accounts that are due (money_settings.usage_due_at), then queues the usage
// orders whose next charge is due (orders.next_collection_attempt_at) as
// order_collection operations, which their own jobs charge. Both reads are
// indexed on the due instant, so an idle pass reads no row. Collection waits
// for limited mode to end. A failed account or order stays due and fails the
// job once the others are done.
type UsageOrderWorker struct {
	WorkerDefaults[money.UsageOrderArgs]
	DB     *db.DB
	Money  *money.MoneyService
	Config *config.Config
	Clock  clockwork.Clock
}

func (UsageOrderWorker) Kind() string { return money.KindUsageOrders }

func (w UsageOrderWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w UsageOrderWorker) Work(ctx context.Context, job *river.Job[money.UsageOrderArgs]) error {
	if w.DB == nil || w.Money == nil || w.Config == nil || !w.Config.UsageBillingEnabled {
		return nil
	}
	logger := log.WithContext(ctx).WithField("worker", money.KindUsageOrders)
	now := w.now()
	var failed error
	if job.Args.Collect == nil {
		next, err := w.bill(ctx, logger, now, job.Args.Bill)
		failed = err
		if next != nil {
			return errors.Join(failed, continuePass(ctx, money.UsageOrderArgs{Bill: next}))
		}
	}
	if config.IsLimitedMode(w.Config) {
		return failed
	}
	next, err := w.collect(ctx, logger, now, job.Args.Collect)
	failed = errors.Join(failed, err)
	if next != nil {
		return errors.Join(failed, continuePass(ctx, money.UsageOrderArgs{Collect: next}))
	}
	return failed
}

// bill checks one page of due usage accounts after after; it returns where
// the next page starts when this one was full.
func (w UsageOrderWorker) bill(ctx context.Context, logger *log.Entry, now time.Time, after *money.UsageAccountKey) (*money.UsageAccountKey, error) {
	args := gen.ListDueUsageAccountsParams{Now: now, RowLimit: usagePage}
	if after != nil {
		args.AfterAt, args.AfterMerchantID, args.AfterCustomerID, args.AfterCurrency = &after.DueAt, &after.MerchantID, &after.CustomerID, &after.Currency
	}
	due, err := w.DB.GenDirectory().ListDueUsageAccounts(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("list due usage accounts: %w", err)
	}
	var failed error
	billed := 0
	for _, row := range due {
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "usage billing", func(ctx context.Context) error {
			ok, err := w.Money.BillDueUsageAccount(ctx, row.CustomerID, row.Currency, now)
			if ok {
				billed++
			}
			return err
		})
		if err != nil {
			logger.WithError(err).WithFields(log.Fields{"merchant_id": row.MerchantID, "customer_id": row.CustomerID, "currency": row.Currency}).
				Error("usage billing: account failed; continuing")
			failed = errors.Join(failed, fmt.Errorf("usage account %s/%s/%s: %w", row.MerchantID, row.CustomerID, row.Currency, err))
		}
		if ctx.Err() != nil {
			return nil, errors.Join(failed, ctx.Err())
		}
	}
	if billed > 0 {
		logger.WithField("orders", billed).Info("usage orders billed")
	}
	if len(due) < usagePage {
		return nil, failed
	}
	last := due[len(due)-1]
	return &money.UsageAccountKey{DueAt: last.UsageDueAt, MerchantID: last.MerchantID, CustomerID: last.CustomerID, Currency: last.Currency}, failed
}

// collect queues one page of due collections after after; it returns where
// the next page starts when this one was full.
func (w UsageOrderWorker) collect(ctx context.Context, logger *log.Entry, now time.Time, after *money.UsageOrderKey) (*money.UsageOrderKey, error) {
	args := gen.ListDueUsageCollectionsParams{Now: now, RowLimit: usagePage}
	if after != nil {
		args.AfterAt, args.AfterMerchantID, args.AfterID = &after.DueAt, &after.MerchantID, &after.OrderID
	}
	due, err := w.DB.GenDirectory().ListDueUsageCollections(ctx, args)
	if err != nil {
		return nil, fmt.Errorf("list due usage collections: %w", err)
	}
	var failed error
	for _, row := range due {
		err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(row.MerchantID), "usage collection", func(ctx context.Context) error {
			return w.Money.CollectDueUsageOrder(ctx, identity.CustomerID(row.CustomerID), row.ID)
		})
		if err != nil {
			logger.WithError(err).WithFields(log.Fields{"merchant_id": row.MerchantID, "order_id": row.ID}).
				Error("usage collection: order failed; continuing")
			failed = errors.Join(failed, fmt.Errorf("usage order %s/%s: %w", row.MerchantID, row.ID, err))
		}
		if ctx.Err() != nil {
			return nil, errors.Join(failed, ctx.Err())
		}
	}
	if len(due) < usagePage {
		return nil, failed
	}
	last := due[len(due)-1]
	return &money.UsageOrderKey{DueAt: last.NextCollectionAttemptAt, MerchantID: last.MerchantID, OrderID: last.ID}, failed
}
