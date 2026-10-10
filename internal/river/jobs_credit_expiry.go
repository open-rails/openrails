package riverjobs

import (
	"context"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindCreditExpiry = "openrails.credit_expiry"

type CreditExpiryArgs struct{}

func (CreditExpiryArgs) Kind() string { return KindCreditExpiry }

// CreditExpiryWorker claws back the unspent remainder of lapsed credit lots:
// for every (merchant, customer, currency) with a past-expiry lot it runs
// grants.ExpireLapsed, one ledger transfer (DR customer_balance / CR
// expired_credits) per lot, idempotent because a clawed lot has zero
// remainder. ExpireLapsed takes the per-customer spend lock inside this
// worker's tx, so an expiry never races a spend.
type CreditExpiryWorker struct {
	river.WorkerDefaults[CreditExpiryArgs]
	DB        *db.DB
	Clock     clockwork.Clock
	BatchSize int
}

func (CreditExpiryWorker) Kind() string { return KindCreditExpiry }

func (w CreditExpiryWorker) Work(ctx context.Context, job *river.Job[CreditExpiryArgs]) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	batchSize := w.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}

	now := clock.Now().UTC()
	nowFn := func() time.Time { return now }
	logger := log.WithContext(ctx).WithField("worker", KindCreditExpiry)

	batchSize32, _ := safecast.Convert[int32](batchSize)

	// The lapsed-lot work queue yields merchant ids only; the customer list and
	// the claw-back run inside each merchant's own scope.
	merchantIDs, err := w.DB.GenDirectory().ListLapsedCreditLotMerchants(ctx, gen.ListLapsedCreditLotMerchantsParams{
		AsOf: now, MerchantLimit: batchSize32,
	})
	if err != nil {
		return fmt.Errorf("credit expiry: list merchants with lapsed credit lots: %w", err)
	}

	expired := map[string]int64{}
	var customers int
	for _, merchantID := range merchantIDs {
		progress.Mark(ctx, "credit expiry merchant "+merchantID.String())
		if err := w.DB.RunInMerchantScope(ctx, billing.MerchantID(merchantID), "credit expiry sweep", func(ctx context.Context) error {
			scopeMerchantID, scopeErr := merchant.Require(ctx)
			if scopeErr != nil {
				return scopeErr
			}
			rows, err := w.DB.Gen(ctx).ListCustomersWithLapsedCreditLots(ctx, gen.ListCustomersWithLapsedCreditLotsParams{
				MerchantID: scopeMerchantID.UUID(),
				AsOf:       now, BatchSize: batchSize32,
			})
			if err != nil {
				return fmt.Errorf("list customers with lapsed credit lots: %w", err)
			}
			customers += len(rows)
			for _, r := range rows {
				currency := ""
				if r.Currency != nil {
					currency = *r.Currency
				}
				if err := w.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					gl := grants.New(gen.New(tx), r.MerchantID)
					gl.SetClock(nowFn)
					amount, e := gl.ExpireLapsed(ctx, r.CustomerID, currency)
					if e != nil {
						return e
					}
					if amount != 0 {
						expired[currency] += amount
					}
					return nil
				}); err != nil {
					return fmt.Errorf("expire lapsed lots for customer %s: %w", r.CustomerID, err)
				}
			}
			return nil
		}); err != nil {
			// One merchant's failure must not abort the rest of the sweep.
			logger.WithError(err).WithField("merchant_id", merchantID).
				Error("credit expiry: merchant pass failed; continuing")
			continue
		}
	}
	if len(expired) > 0 {
		logger.WithFields(log.Fields{
			"merchants": len(merchantIDs), "customers": customers, "expired": moneyutil.FormatAmounts(expired),
		}).Info("clawed back lapsed credit-lot remainders")
	}

	return nil
}
