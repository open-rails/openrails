package riverjobs

import (
	"context"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const KindCreditExpiry = "openrails.credit_expiry"

type CreditExpiryArgs struct{}

func (CreditExpiryArgs) Kind() string { return KindCreditExpiry }

// CreditExpiryWorker claws back the unspent remainder of lapsed credit lots
// (#514): for every (merchant, customer, currency) with a past-expiry credit
// grant that still has an unspent balance, it runs grants.ExpireLapsed, which
// emits a #512 ledger transfer (DR customer_balance / CR expired_credits) per
// lapsed lot — conserved, append-only, idempotent (an already-clawed lot has
// zero remainder and is skipped). ExpireLapsed takes the per-customer spend
// lock inside this worker's tx (#677), so an expiry never races a spend on the
// same lot. The credit lot IS the grant; there is no money_blocks table to
// compact anymore.
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

	// or#868 B1: this used to enumerate customers with a bare `RunInTx` on the
	// base pool, under a comment calling it a "privileged (no-GUC) cross-merchant
	// sweep". There is no privileged pool — grants and customers both FORCE RLS,
	// so with no app.merchant_id the enumeration matched `merchant_id = NULL` and
	// returned nothing, every run. This worker had NEVER expired a credit lot:
	// lots lapsed and customers kept spending them. Enumerate the merchants
	// through 0022's SECURITY DEFINER work queue (ids only; it RAISES if its
	// definer cannot bypass RLS), then do the real work — the per-customer list
	// AND the ledger claw-back — inside each merchant's own scope.
	merchantIDs, err := w.DB.GenDirectory().ListLapsedCreditLotMerchants(ctx, gen.ListLapsedCreditLotMerchantsParams{
		AsOf: now, MerchantLimit: batchSize32,
	})
	if err != nil {
		return fmt.Errorf("credit expiry: list merchants with lapsed credit lots: %w", err)
	}

	var totalExpired int64
	var customers int
	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := *mid
		if err := w.DB.RunInMerchantScope(ctx, merchant.ID(merchantID), "credit expiry sweep", func(ctx context.Context) error {
			rows, err := w.DB.Gen(ctx).ListCustomersWithLapsedCreditLots(ctx, gen.ListCustomersWithLapsedCreditLotsParams{
				AsOf: now, BatchSize: batchSize32,
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
					expired, e := gl.ExpireLapsed(ctx, r.CustomerID, currency)
					if e != nil {
						return e
					}
					totalExpired += expired
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
	if totalExpired > 0 {
		logger.WithFields(log.Fields{
			"merchants": len(merchantIDs), "customers": customers, "expired_amount": totalExpired,
		}).Info("clawed back lapsed credit-lot remainders")
	}

	return nil
}
