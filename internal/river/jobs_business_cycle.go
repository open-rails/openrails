package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/business"
	"github.com/open-rails/openrails/pkg/merchant"
)

const (
	KindBusinessCycle = "openrails.business_cycle"

	// businessCycleMerchantBatch caps one pass's fan-out; the work queue is
	// indexed on onboarded business profiles, so a pass costs the size of the
	// B2B book, never the merchant directory.
	businessCycleMerchantBatch = 500
)

// BusinessCycleArgs triggers one or#910 dunning + budget-alert pass.
type BusinessCycleArgs struct{}

func (BusinessCycleArgs) Kind() string { return KindBusinessCycle }

// BusinessCycleWorker walks every merchant with onboarded business customers
// and runs the notify-only dunning ladder + budget alerts (or#910). Like the
// delinquency worker, it moves no money and calls no provider — it reads
// invoice/pending truth and writes notices, recommendation watermarks and
// host signals, so it runs even when no charger is armed.
type BusinessCycleWorker struct {
	river.WorkerDefaults[BusinessCycleArgs]
	DB    *db.DB
	Clock clockwork.Clock
}

func (BusinessCycleWorker) Kind() string { return KindBusinessCycle }

func (w BusinessCycleWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

func (w BusinessCycleWorker) Work(ctx context.Context, _ *river.Job[BusinessCycleArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindBusinessCycle)
	if w.DB == nil {
		logger.Debug("db not configured; skipping business cycle")
		return nil
	}
	now := w.now()

	// GenDirectory: the SECURITY DEFINER work queue is the ONE sanctioned
	// cross-merchant read here, and it returns ids only (FC-16 R2).
	merchantIDs, err := w.DB.GenDirectory().ListBusinessCycleWorkMerchants(ctx, businessCycleMerchantBatch)
	if err != nil {
		return fmt.Errorf("list merchants with business-cycle work: %w", err)
	}
	if len(merchantIDs) == 0 {
		logger.Debug("Business cycle: no merchant has onboarded business customers")
		return nil
	}

	svc := business.NewService(w.DB, w.Clock)
	var payers, notices, recommendations, clearances int
	for _, mid := range merchantIDs {
		if mid == nil {
			continue
		}
		merchantID := merchant.ID(*mid)
		if err := w.DB.RunInMerchantScope(ctx, merchantID, "business dunning cycle", func(mctx context.Context) error {
			res, err := svc.EvaluateMerchant(mctx, now)
			payers += res.Payers
			notices += res.NoticesEmitted
			recommendations += res.Recommendations
			clearances += res.Clearances
			return err
		}); err != nil {
			// One merchant's failure must not abort the rest of the run.
			logger.WithError(err).WithField("merchant_id", merchantID.String()).
				Error("Business cycle: merchant pass failed; continuing")
		}
	}
	if notices > 0 || recommendations > 0 || clearances > 0 {
		logger.WithFields(log.Fields{
			"merchants": len(merchantIDs), "payers": payers, "notices": notices,
			"suspension_recommendations": recommendations, "clearances": clearances,
		}).Info("Business cycle pass complete")
	}
	return nil
}
