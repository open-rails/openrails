package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/shared/progress"
	"github.com/open-rails/openrails/internal/writeposture"
)

const KindConvergeSweep = "openrails.converge_sweep"

type ConvergeSweepArgs struct{}

func (ConvergeSweepArgs) Kind() string { return KindConvergeSweep }

// ConvergeSweepWorker runs the converge engine for every active merchant on a
// schedule, catching drift no request path touched. One merchant's failure
// never aborts the sweep.
type ConvergeSweepWorker struct {
	river.WorkerDefaults[ConvergeSweepArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// Alerts sends requires_review findings to the operator notification
	// store. nil = no-op.
	Alerts *alerting.Service
}

func (ConvergeSweepWorker) Kind() string { return KindConvergeSweep }

func (w ConvergeSweepWorker) Work(ctx context.Context, job *river.Job[ConvergeSweepArgs]) error {
	// Readonly mode must make the sweep an observer: it cancels and closes
	// entitlement windows across every merchant.
	if w.Config != nil && config.IsProviderReadOnly(w.Config) {
		log.WithContext(ctx).WithField("worker", KindConvergeSweep).
			Warn("Readonly mode: converge sweep skipped (pure observer; no local convergence)")
		return nil
	}
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	engine := converge.NewConvergeEngine(w.DB, clock)
	engine.Now = func() time.Time { return clock.Now().UTC() }
	if w.Alerts != nil {
		// A nil *alerting.Service boxed into the FindingNotifier interface
		// would panic on first use.
		engine.Notifier = w.Alerts
	}
	logger := log.WithContext(ctx).WithField("worker", KindConvergeSweep)

	// merchants is the global directory; per-merchant work runs inside
	// RunInMerchantConn.
	merchantIDs, err := w.DB.GenDirectory().ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("converge sweep: list merchants: %w", err)
	}

	// The kill switch is read per merchant: one merchant's stop does not halt
	// the fleet, and the fleet-wide stop halts every merchant.
	gate := destructive.New(w.DB)
	postures := writeposture.View{Config: w.Config, DB: w.DB}

	var swept, findings, autoFixed, reconcileRequired, adminRequired, gated int
	for _, mid := range merchantIDs {
		progress.Mark(ctx, "converge sweep merchant "+mid.String())
		mctx := merchant.WithID(ctx, billing.MerchantID(mid))
		var res converge.ConvergeResult
		var blocked string
		if err := w.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
			if v := gate.Check(ctx, mid); !v.Allowed {
				blocked = v.Reason
				return nil
			}
			if p := postures.Posture(ctx, mid); p.ReadOnly() {
				blocked = p.Reason
				return nil
			}
			var e error
			res, e = engine.Converge(ctx, converge.Scope{Merchant: billing.MerchantID(mid)})
			return e
		}); err != nil {
			// One merchant's failure must not abort the rest of the sweep.
			logger.WithError(err).WithField("merchant_id", mid).
				Error("converge sweep: merchant failed; continuing")
			continue
		}
		if blocked != "" {
			gated++
			logger.WithField("merchant_id", mid).Warn("converge sweep: destructive actions gated — " + blocked)
			continue
		}
		swept++
		findings += res.Findings
		autoFixed += res.AutoFixed
		reconcileRequired += res.ReconcileRequired
		adminRequired += res.AdminRequired
	}
	if findings > 0 || gated > 0 {
		logger.WithFields(log.Fields{
			"merchants": swept, "gated": gated, "findings": findings, "auto_fixed": autoFixed,
			"reconcile_required": reconcileRequired, "admin_required": adminRequired,
		}).Info("converge sweep completed")
	}
	return nil
}
