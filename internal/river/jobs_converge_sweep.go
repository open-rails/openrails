package riverjobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/pkg/merchant"
)

const KindConvergeSweep = "openrails.converge_sweep"

type ConvergeSweepArgs struct{}

func (ConvergeSweepArgs) Kind() string { return KindConvergeSweep }

// ConvergeSweepWorker drives the #511 Convergence Engine on a schedule: for every
// active merchant it runs reconcile.Converge(merchant) — the idempotent
// internal-plane pass (DERIVE → LIFE → CON) that converges grant effects,
// subscription lifecycle and internal consistency to their correct CURRENT state.
// It is the background invocation of the very same engine the request path calls
// inline after a mutation, so drift no inline path touched — a dunning schedule
// that stalled, a grace window that elapsed while the account sat idle, a checkout
// abandoned without a webhook — is still detected and remediated within one sweep
// interval. Clean merchants cost a no-op (Converge does zero writes when nothing
// drifted), so the sweep is cheap to run often.
//
// Cross-merchant by design: the merchant directory is read on a privileged no-GUC
// connection (merchants is a control-plane table, not RLS-scoped), then each
// merchant's Converge runs inside its own RunInMerchantConn so all detection and
// repair is RLS-scoped to that merchant. One merchant's failure is logged and
// skipped — it must never abort the sweep for the rest.
type ConvergeSweepWorker struct {
	river.WorkerDefaults[ConvergeSweepArgs]
	DB     *db.DB
	Config *config.Config
	Clock  clockwork.Clock
	// Alerts bridges requires_review findings into the #736 operator
	// notification store (#787). nil = no-op (no alerting service wired).
	Alerts *alerting.Service
}

func (ConvergeSweepWorker) Kind() string { return KindConvergeSweep }

func (w ConvergeSweepWorker) Work(ctx context.Context, job *river.Job[ConvergeSweepArgs]) error {
	clock := w.Clock
	if clock == nil {
		clock = clockwork.NewRealClock()
	}
	engine := converge.NewConvergeEngine(w.DB)
	engine.Now = func() time.Time { return clock.Now().UTC() }
	if w.Alerts != nil {
		// #787: nil-check before assigning to the interface field — a nil
		// *alerting.Service boxed into a non-nil FindingNotifier interface
		// would panic on first use.
		engine.Notifier = w.Alerts
	}
	logger := log.WithContext(ctx).WithField("worker", KindConvergeSweep)

	// Privileged (no-GUC) read of the control-plane merchant directory.
	merchantIDs, err := w.DB.Gen(ctx).ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("converge sweep: list merchants: %w", err)
	}

	var swept, findings, autoFixed, reconcileRequired, adminRequired int
	for _, mid := range merchantIDs {
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		var res converge.ConvergeResult
		if err := w.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
			var e error
			res, e = engine.Converge(ctx, converge.Scope{Merchant: merchant.ID(mid)})
			return e
		}); err != nil {
			// One merchant's failure must not abort the rest of the sweep.
			logger.WithError(err).WithField("merchant_id", mid).
				Error("converge sweep: merchant failed; continuing")
			continue
		}
		swept++
		findings += res.Findings
		autoFixed += res.AutoFixed
		reconcileRequired += res.ReconcileRequired
		adminRequired += res.AdminRequired
	}
	if findings > 0 {
		logger.WithFields(log.Fields{
			"merchants": swept, "findings": findings, "auto_fixed": autoFixed,
			"reconcile_required": reconcileRequired, "admin_required": adminRequired,
		}).Info("converge sweep completed")
	}
	return nil
}
