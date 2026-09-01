package riverjobs

import (
	"context"
	"fmt"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindAlertEval = "openrails.alert_eval"

// AlertEvalArgs triggers one alerting evaluation pass (#736): select merchants
// with enabled rules (indexed — work scales with rules on file, not the merchant
// directory), and for each run its rules under merchant context, edge-triggering
// threshold crossings and emitting due digests.
type AlertEvalArgs struct{}

func (AlertEvalArgs) Kind() string { return KindAlertEval }

// AlertEvalWorker is the slow-cadence alerting evaluator. Cross-merchant
// selection runs on the base pool (same posture as the #358 intent executor
// sweeps); each merchant's evaluation is RLS-scoped inside EvaluateMerchant.
// One merchant's failure is logged and skipped — never aborts the sweep.
type AlertEvalWorker struct {
	river.WorkerDefaults[AlertEvalArgs]
	DB     *db.DB
	Alerts *alerting.Service
	Clock  clockwork.Clock
}

func (AlertEvalWorker) Kind() string { return KindAlertEval }

func (w AlertEvalWorker) Work(ctx context.Context, _ *river.Job[AlertEvalArgs]) error {
	if w.Alerts == nil {
		// No alerting service wired (worker-only runtime without metrics): no-op.
		return nil
	}
	merchantIDs, err := w.Alerts.ArmedMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("alert eval: list armed merchants: %w", err)
	}
	logger := log.WithContext(ctx).WithField("worker", KindAlertEval)

	var total alerting.EvalStats
	var swept int
	for _, mid := range merchantIDs {
		progress.Mark(ctx, "alert eval merchant "+mid.String())
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		stats, err := w.Alerts.EvaluateMerchant(mctx)
		if err != nil {
			logger.WithError(err).WithField("merchant_id", mid).
				Error("alert eval: merchant failed; continuing")
			continue
		}
		swept++
		total.Evaluated += stats.Evaluated
		total.Fired += stats.Fired
		total.Cleared += stats.Cleared
		total.Digests += stats.Digests
		total.Errors += stats.Errors
	}
	if total.Fired > 0 || total.Cleared > 0 || total.Digests > 0 || total.Errors > 0 {
		logger.WithFields(log.Fields{
			"merchants": swept, "evaluated": total.Evaluated, "fired": total.Fired,
			"cleared": total.Cleared, "digests": total.Digests, "errors": total.Errors,
		}).Info("alert evaluation pass completed")
	}
	return nil
}
