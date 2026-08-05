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
	"github.com/open-rails/openrails/internal/destructive"
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
	// #836: the sweep had NO readonly check at all, unlike the refresh and
	// dunning workers — so `provider_write_mode: readonly` did not make it an
	// observer. Its pending_stale branch cancels and its grant_effect.mismatch
	// branch closes entitlement windows, every 15 minutes, RunOnStart, across
	// every active merchant.
	if w.Config != nil && w.Config.IsProviderReadOnly() {
		log.WithContext(ctx).WithField("worker", KindConvergeSweep).
			Warn("Readonly mode: converge sweep skipped (pure observer; no local convergence)")
		return nil
	}
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

	// openrails.merchants is the policy-free directory, so the base pool
	// genuinely answers this; the per-merchant work below runs inside
	// RunInMerchantConn. Not a privilege — there is no privileged pool (or#868).
	merchantIDs, err := w.DB.GenDirectory().ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("converge sweep: list merchants: %w", err)
	}

	// #836: the runtime kill switch, read per merchant so one merchant's stop
	// does not halt the fleet and the fleet-wide stop halts every merchant.
	gate := destructive.New(w.DB)

	var swept, findings, autoFixed, reconcileRequired, adminRequired, gated int
	for _, mid := range merchantIDs {
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		var res converge.ConvergeResult
		var blocked string
		if err := w.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
			if v := gate.Check(ctx, mid); !v.Allowed {
				blocked = v.Reason
				return nil
			}
			var e error
			res, e = engine.Converge(ctx, converge.Scope{Merchant: merchant.ID(mid)})
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
