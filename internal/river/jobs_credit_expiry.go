package riverjobs

import (
	"context"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/grants"
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
// zero remainder and is skipped). The credit lot IS the grant; there is no
// money_blocks table to compact anymore.
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
	// Privileged (no-GUC) cross-merchant sweep: find customers with lapsed,
	// not-yet-clawed credit lots, then expire each via the grant ledger.
	var rows []gen.ListCustomersWithLapsedCreditLotsRow
	if err := w.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = gen.New(tx).ListCustomersWithLapsedCreditLots(ctx, gen.ListCustomersWithLapsedCreditLotsParams{
			AsOf: now, BatchSize: batchSize32,
		})
		return err
	}); err != nil {
		return err
	}

	var totalExpired int64
	for _, r := range rows {
		currency := ""
		if r.Currency != nil {
			currency = *r.Currency
		}
		var expired int64
		if err := w.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			gl := grants.New(gen.New(tx), r.MerchantID)
			gl.SetClock(nowFn)
			var e error
			expired, e = gl.ExpireLapsed(ctx, r.CustomerID, currency)
			return e
		}); err != nil {
			return err
		}
		totalExpired += expired
	}
	if totalExpired > 0 {
		logger.WithFields(log.Fields{"customers": len(rows), "expired_amount": totalExpired}).
			Info("clawed back lapsed credit-lot remainders")
	}

	return nil
}
