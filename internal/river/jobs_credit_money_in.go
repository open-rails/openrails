package riverjobs

import (
	"context"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

// These workers drive the credit money-in + reconciliation flows (issues
// #239/#240/#241/#243). The auto-top-up and arrears workers need a
// credits.Charger (off-session processor charge) and the low-balance worker
// needs a credits.Alerter; when those are not configured the worker logs and
// no-ops so it is safe to register before the processor/notification wiring
// lands. The reconcile worker needs no external dependency and runs fully.

// --- Low-balance alerts (#240) ---

const KindLowBalanceAlert = "billing.low_balance_alert"

type LowBalanceAlertArgs struct{}

func (LowBalanceAlertArgs) Kind() string { return KindLowBalanceAlert }

type LowBalanceAlertWorker struct {
	river.WorkerDefaults[LowBalanceAlertArgs]
	Credits  *credits.CreditsService
	Alerter  credits.Alerter
	Cooldown time.Duration
}

func (LowBalanceAlertWorker) Kind() string { return KindLowBalanceAlert }

func (w LowBalanceAlertWorker) Work(ctx context.Context, _ *river.Job[LowBalanceAlertArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindLowBalanceAlert)
	if w.Credits == nil || w.Alerter == nil {
		logger.Debug("low-balance alerter not configured; skipping")
		return nil
	}
	cooldown := w.Cooldown
	if cooldown <= 0 {
		cooldown = 24 * time.Hour
	}
	n, err := w.Credits.RunLowBalanceAlerts(ctx, w.Alerter, cooldown)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.WithField("alerts_sent", n).Info("low-balance alerts sent")
	}
	return nil
}

// --- Prepaid auto-top-up (#239) ---

const KindAutoTopup = "billing.auto_topup"

type AutoTopupArgs struct{}

func (AutoTopupArgs) Kind() string { return KindAutoTopup }

type AutoTopupWorker struct {
	river.WorkerDefaults[AutoTopupArgs]
	Credits  *credits.CreditsService
	Charger  credits.Charger
	Cooldown time.Duration
}

func (AutoTopupWorker) Kind() string { return KindAutoTopup }

func (w AutoTopupWorker) Work(ctx context.Context, _ *river.Job[AutoTopupArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindAutoTopup)
	if w.Credits == nil || w.Charger == nil {
		logger.Debug("auto-top-up charger not configured; skipping")
		return nil
	}
	cooldown := w.Cooldown
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	n, err := w.Credits.RunAutoTopups(ctx, w.Charger, cooldown)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.WithField("topups", n).Info("auto top-ups completed")
	}
	return nil
}

// --- Arrears collection (#241) ---

const KindArrearsCharge = "billing.arrears_charge"

// Arrears collection cadence (#241/#301). The HOURLY job collects balances at or
// above ArrearsHourlyThresholdCents (collect big balances promptly); the MONTHLY
// sweep collects everything at or above ArrearsMonthlyFloorCents (the $1 floor,
// so we don't burn processor fees on dust). "Whichever comes first." Charges are
// idempotent per owed-snapshot, so the two cadences never double-collect.
// TODO(#301): make these configurable per-tenant; decide calendar-month vs
// fixed-interval boundary.
const (
	ArrearsHourlyThresholdCents = 5000 // $50
	ArrearsMonthlyFloorCents    = 100  // $1
)

// ArrearsChargeArgs carries the per-run collection threshold so one worker serves
// both the hourly threshold trigger and the monthly $1-floor sweep.
type ArrearsChargeArgs struct {
	ThresholdCents int64 `json:"threshold_cents"`
}

func (ArrearsChargeArgs) Kind() string { return KindArrearsCharge }

type ArrearsChargeWorker struct {
	river.WorkerDefaults[ArrearsChargeArgs]
	Credits *credits.CreditsService
	Charger credits.Charger
}

func (ArrearsChargeWorker) Kind() string { return KindArrearsCharge }

func (w ArrearsChargeWorker) Work(ctx context.Context, job *river.Job[ArrearsChargeArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindArrearsCharge)
	if w.Credits == nil || w.Charger == nil {
		logger.Debug("arrears charger not configured; skipping")
		return nil
	}
	n, err := w.Credits.ChargeOutstanding(ctx, w.Charger, job.Args.ThresholdCents)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.WithField("charged", n).Info("arrears charges completed")
	}
	return nil
}

// --- Ledger reconciliation (#243) ---

const KindCreditReconcile = "billing.credit_reconcile"

type CreditReconcileArgs struct{}

func (CreditReconcileArgs) Kind() string { return KindCreditReconcile }

type CreditReconcileWorker struct {
	river.WorkerDefaults[CreditReconcileArgs]
	Credits *credits.CreditsService
	Clock   clockwork.Clock
}

func (CreditReconcileWorker) Kind() string { return KindCreditReconcile }

func (w CreditReconcileWorker) Work(ctx context.Context, _ *river.Job[CreditReconcileArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindCreditReconcile)
	if w.Credits == nil {
		logger.Debug("credits service not configured; skipping reconcile")
		return nil
	}
	rep, err := w.Credits.Reconcile(ctx)
	if err != nil {
		return err
	}
	if len(rep.OrphanedHolds) > 0 || len(rep.HeldBalanceDrift) > 0 || len(rep.BalanceAnomalies) > 0 {
		logger.WithFields(log.Fields{
			"orphaned_holds":     len(rep.OrphanedHolds),
			"held_balance_drift": len(rep.HeldBalanceDrift),
			"balance_anomalies":  len(rep.BalanceAnomalies),
		}).Warn("credit ledger reconciliation found inconsistencies (alert-only)")
	}
	return nil
}
