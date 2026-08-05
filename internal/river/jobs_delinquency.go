package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	KindDelinquency = "openrails.delinquency"

	// delinquencyMerchantBatch caps one pass's fan-out. The work queue is
	// indexed on the work itself (overdue receivables / already-parked payers),
	// so this bounds a pass by ACTIVITY, never by the merchant directory.
	delinquencyMerchantBatch = 500
)

// DelinquencyArgs triggers one arrears delinquency evaluation pass.
type DelinquencyArgs struct{}

func (DelinquencyArgs) Kind() string { return KindDelinquency }

// DelinquencyWorker evaluates arrears delinquency and emits the host signal
// (or#878).
//
// It is deliberately its OWN job rather than a leg of the hourly invoice pass:
//
//   - it must run when no charger is armed and in limited/readonly mode, because
//     it moves no money and calls no provider — it reads invoices and writes
//     local state;
//   - the EXIT half is latency-sensitive in the direction that hurts customers.
//     A cleared debt that goes unnoticed is someone who paid still being told
//     they cannot spend, so this runs on a tighter cadence than collection.
type DelinquencyWorker struct {
	river.WorkerDefaults[DelinquencyArgs]
	DB    *db.DB
	Clock clockwork.Clock
}

func (DelinquencyWorker) Kind() string { return KindDelinquency }

func (w DelinquencyWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w DelinquencyWorker) Work(ctx context.Context, _ *river.Job[DelinquencyArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindDelinquency)
	if w.DB == nil {
		logger.Debug("db not configured; skipping delinquency evaluation")
		return nil
	}
	now := w.now()

	// GenDirectory: the SECURITY DEFINER work queue is the ONE sanctioned
	// cross-merchant read here, and it returns ids only (FC-16 R2).
	merchantIDs, err := w.DB.GenDirectory().ListDelinquencyWorkMerchants(ctx, gen.ListDelinquencyWorkMerchantsParams{
		Now: now, MerchantLimit: delinquencyMerchantBatch,
	})
	if err != nil {
		return fmt.Errorf("list merchants with delinquency work: %w", err)
	}
	if len(merchantIDs) == 0 {
		logger.Debug("Delinquency: no merchant has overdue or parked receivables")
		return nil
	}

	svc := delinquency.NewService(w.DB, w.Clock)
	evaluated, transitions := 0, 0
	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := merchant.ID(*mid)
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "delinquency evaluation", func(mctx context.Context) error {
			res, err := svc.Evaluate(mctx, now)
			evaluated += res.Evaluated
			transitions += len(res.Transitions)
			for _, t := range res.Transitions {
				// One line per transition, because this is the decision an
				// operator will be asked to explain: why this customer, when,
				// and in which direction.
				log.WithContext(mctx).WithFields(log.Fields{
					"merchant_id": merchantID.String(),
					"customer_id": t.CustomerID.String(),
					"currency":    t.Currency,
					"from_state":  t.From.String(),
					"to_state":    t.To.String(),
				}).Info("Delinquency: state transition recorded and signalled to the host")
			}
			return err
		}); err != nil {
			// One merchant's failure must not abort the rest of the run.
			logger.WithError(err).WithField("merchant_id", merchantID.String()).
				Error("Delinquency: merchant pass failed; continuing")
		}
	}
	if transitions > 0 {
		logger.WithFields(log.Fields{
			"merchants": len(merchantIDs), "evaluated": evaluated, "transitions": transitions,
		}).Info("Delinquency evaluation pass complete")
	}
	return nil
}
