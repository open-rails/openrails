package riverjobs

import (
	"context"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

// These workers drive the money-in + reconciliation flows (issues
// #239/#240/#241/#243). The auto-top-up and arrears workers need a
// money.Charger (off-session processor charge) and the low-balance worker
// needs a money.Alerter; when those are not configured the worker logs and
// no-ops so it is safe to register before the processor/notification wiring
// lands. The reconcile worker needs no external dependency and runs fully.

// --- Low-balance alerts (#240) ---

const KindLowBalanceAlert = "openrails.low_balance_alert"

type LowBalanceAlertArgs struct{}

func (LowBalanceAlertArgs) Kind() string { return KindLowBalanceAlert }

type LowBalanceAlertWorker struct {
	river.WorkerDefaults[LowBalanceAlertArgs]
	Money    *money.MoneyService
	Alerter  money.Alerter
	Cooldown time.Duration
}

func (LowBalanceAlertWorker) Kind() string { return KindLowBalanceAlert }

func (w LowBalanceAlertWorker) Work(ctx context.Context, _ *river.Job[LowBalanceAlertArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindLowBalanceAlert)
	if w.Money == nil || w.Alerter == nil {
		logger.Debug("low-balance alerter not configured; skipping")
		return nil
	}
	cooldown := w.Cooldown
	if cooldown <= 0 {
		cooldown = 24 * time.Hour
	}
	n, err := w.Money.RunLowBalanceAlerts(ctx, w.Alerter, cooldown)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.WithField("alerts_sent", n).Info("low-balance alerts sent")
	}
	return nil
}

// --- Prepaid auto-top-up (#239) ---

const KindAutoTopup = "openrails.auto_topup"

type AutoTopupArgs struct{}

func (AutoTopupArgs) Kind() string { return KindAutoTopup }

type AutoTopupWorker struct {
	river.WorkerDefaults[AutoTopupArgs]
	Money    *money.MoneyService
	Charger  money.Charger
	Config   *config.Config
	Cooldown time.Duration
}

func (AutoTopupWorker) Kind() string { return KindAutoTopup }

func (w AutoTopupWorker) Work(ctx context.Context, _ *river.Job[AutoTopupArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindAutoTopup)
	if w.Config != nil && w.Config.IsLimitedMode() {
		logger.Warn("limited mode: skipping auto-top-up charges (#345)")
		return nil
	}
	if w.Money == nil || w.Charger == nil {
		logger.Debug("auto-top-up charger not configured; skipping")
		return nil
	}
	cooldown := w.Cooldown
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	n, err := w.Money.RunAutoTopups(ctx, w.Charger, cooldown)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.WithField("topups", n).Info("auto top-ups completed")
	}
	return nil
}

// --- Arrears collection (#241) ---

const KindArrearsCharge = "openrails.arrears_charge"

// Arrears collection cadence (#241/#301). The HOURLY job collects balances at or
// above ArrearsHourlyThresholdMicros (collect big balances promptly); the MONTHLY
// sweep collects everything at or above ArrearsMonthlyFloorMicros (the $1 floor,
// so we don't burn processor fees on dust). "Whichever comes first." Charges are
// idempotent per owed-snapshot, so the two cadences never double-collect.
// TODO(#301): make these configurable per-tenant; decide calendar-month vs
// fixed-interval boundary.
const (
	ArrearsHourlyThresholdMicros = 50_000_000 // $50
	ArrearsMonthlyFloorMicros    = 1_000_000  // $1
)

// ArrearsChargeArgs carries the per-run collection threshold so one worker serves
// both the hourly threshold trigger and the monthly $1-floor sweep.
type ArrearsChargeArgs struct {
	ThresholdMicros int64 `json:"threshold_micros"`
}

func (ArrearsChargeArgs) Kind() string { return KindArrearsCharge }

type ArrearsChargeWorker struct {
	river.WorkerDefaults[ArrearsChargeArgs]
	Money   *money.MoneyService
	Charger money.Charger
	Config  *config.Config
}

func (ArrearsChargeWorker) Kind() string { return KindArrearsCharge }

func (w ArrearsChargeWorker) Work(ctx context.Context, job *river.Job[ArrearsChargeArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindArrearsCharge)
	if w.Config != nil && w.Config.IsLimitedMode() {
		logger.Warn("limited mode: skipping arrears collection charges (#345)")
		return nil
	}
	if w.Money == nil || w.Charger == nil {
		logger.Debug("arrears charger not configured; skipping")
		return nil
	}
	n, err := w.Money.ChargeOutstanding(ctx, w.Charger, job.Args.ThresholdMicros)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.WithField("charged", n).Info("arrears charges completed")
	}
	return nil
}

// --- Monthly itemized invoices (#303) ---

const KindInvoiceFinalize = "openrails.invoice_finalize"

type InvoiceFinalizeArgs struct{}

func (InvoiceFinalizeArgs) Kind() string { return KindInvoiceFinalize }

type InvoiceFinalizeWorker struct {
	river.WorkerDefaults[InvoiceFinalizeArgs]
	Money *money.MoneyService
	Clock clockwork.Clock
}

func (InvoiceFinalizeWorker) Kind() string { return KindInvoiceFinalize }

func (w InvoiceFinalizeWorker) Work(ctx context.Context, _ *river.Job[InvoiceFinalizeArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindInvoiceFinalize)
	if w.Money == nil {
		logger.Debug("money service not configured; skipping invoice finalize")
		return nil
	}
	now := time.Now().UTC()
	if w.Clock != nil {
		now = w.Clock.Now().UTC()
	}
	// Finalize the PREVIOUS calendar month [firstOfPrevMonth, firstOfThisMonth).
	// Idempotent per (payer, period), so running daily is safe and guarantees
	// the prior month is finalized shortly after rollover.
	firstThis := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := firstThis.AddDate(0, -1, 0)
	n, err := w.Money.FinalizeDueInvoices(ctx, from, firstThis)
	if err != nil {
		return err
	}
	if n > 0 {
		logger.WithField("invoices", n).Info("monthly invoices finalized")
	}
	return nil
}

// --- Ledger reconciliation (#243) ---

const KindCreditReconcile = "openrails.credit_reconcile"

type CreditReconcileArgs struct{}

func (CreditReconcileArgs) Kind() string { return KindCreditReconcile }

type CreditReconcileWorker struct {
	river.WorkerDefaults[CreditReconcileArgs]
	Money *money.MoneyService
	Clock clockwork.Clock
}

func (CreditReconcileWorker) Kind() string { return KindCreditReconcile }

func (w CreditReconcileWorker) Work(ctx context.Context, _ *river.Job[CreditReconcileArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindCreditReconcile)
	if w.Money == nil {
		logger.Debug("money service not configured; skipping reconcile")
		return nil
	}
	rep, err := w.Money.Reconcile(ctx)
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
