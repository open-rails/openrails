package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

// forEachActiveMerchant runs fn once per active merchant under a merchant-scoped
// connection (#673): River job contexts carry no merchant, and every money path
// these workers call requires one. Mirrors ConvergeSweepWorker — privileged
// (no-GUC) read of the control-plane merchant directory, then each merchant's
// work runs RLS-scoped inside its own RunInMerchantConn. One merchant's failure
// is logged and does not abort the rest; the joined error is returned so a
// failing run is visible in River instead of silently "succeeding".
func forEachActiveMerchant(ctx context.Context, dbi *db.DB, logger *log.Entry, fn func(ctx context.Context) error) error {
	if dbi == nil {
		logger.Debug("db not configured; skipping")
		return nil
	}
	merchantIDs, err := dbi.Gen(ctx).ListActiveMerchantIDs(ctx)
	if err != nil {
		return fmt.Errorf("list merchants: %w", err)
	}
	var errs []error
	for _, mid := range merchantIDs {
		mctx := merchant.WithID(ctx, merchant.ID(mid))
		if err := dbi.RunInMerchantConn(mctx, fn); err != nil {
			logger.WithError(err).WithField("merchant_id", mid).Error("merchant failed; continuing")
			errs = append(errs, fmt.Errorf("merchant %s: %w", mid, err))
		}
	}
	return errors.Join(errs...)
}

// These workers drive the money-in + reconciliation flows (issues
// #239/#240/#241/#243). The auto-top-up and arrears workers need a
// money.Charger (off-session rail charge) and the low-balance worker
// needs a money.Alerter; when those are not configured the worker logs and
// no-ops so it is safe to register before the rail/notification wiring
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
	DB     *db.DB
	Money  *money.MoneyService
	Config *config.Config
	// Intents is the write-through provider-intent runner (#674): each due
	// episode posts a durable topup_charge intent (keyed off the persisted
	// last_topup_at anchor) and executes it inline; anything ambiguous or
	// parked drains via the scheduled executor/verifier. The charge itself
	// runs inside the topup_charge handler.
	Intents  *intents.Runner
	Cooldown time.Duration
}

func (AutoTopupWorker) Kind() string { return KindAutoTopup }

func (w AutoTopupWorker) Work(ctx context.Context, _ *river.Job[AutoTopupArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindAutoTopup)
	if w.Config != nil && w.Config.IsLimitedMode() {
		logger.Warn("limited mode: skipping auto-top-up charges (#345)")
		return nil
	}
	if w.Money == nil || w.Intents == nil {
		logger.Debug("auto-top-up intent runner not configured; skipping")
		return nil
	}
	cooldown := w.Cooldown
	if cooldown <= 0 {
		cooldown = time.Hour
	}
	// #673: the scan requires a merchant in context; fan out per merchant.
	return forEachActiveMerchant(ctx, w.DB, logger, func(ctx context.Context) error {
		candidates, err := w.Money.ListDueAutoTopups(ctx, cooldown)
		if err != nil {
			return err
		}
		done := 0
		for _, c := range candidates {
			intent, err := w.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{
				MerchantID: c.MerchantID,
				Provider:   c.Rail,
				IntentType: intents.TypeTopupCharge,
				Payload: intents.TopupChargePayload{
					CustomerID:      c.CustomerID,
					Currency:        c.Currency,
					AmountNative:    c.AmountNative,
					PaymentMethodID: c.PaymentMethodID,
					EpisodeAnchor:   c.EpisodeAnchor,
				},
				IdempotencyKey: intents.TopupChargeIdempotencyKey(c.CustomerID, c.Currency, c.EpisodeAnchor),
				NextAttemptAt:  time.Now().UTC(),
				Origin:         intents.OriginSystem,
				OriginReason:   "prepaid auto-top-up (balance below threshold)",
			})
			if err != nil {
				logger.WithError(err).WithField("customer_id", c.CustomerID).Error("auto-topup: post intent failed")
				continue
			}
			if intent.Status == intents.StatusSucceeded {
				done++
			}
		}
		if done > 0 {
			logger.WithField("topups", done).Info("auto top-ups completed")
		}
		return nil
	})
}

// --- Invoices and arrears collection (#241/#301/#303) ---

const KindInvoice = "openrails.invoice"

// InvoiceArgs lets one invoice-domain worker serve the recurring invoice
// lifecycle without splitting each phase into a separate River worker type.
type InvoiceArgs struct {
	Collect                   bool  `json:"collect,omitempty"`
	CollectionThresholdAmount int64 `json:"collection_threshold_amount,omitempty"`
	UseMonthlyFloor           bool  `json:"use_monthly_floor,omitempty"`
	FinalizePreviousMonth     bool  `json:"finalize_previous_month,omitempty"`
}

func (InvoiceArgs) Kind() string { return KindInvoice }

type InvoiceWorker struct {
	river.WorkerDefaults[InvoiceArgs]
	DB      *db.DB
	Money   *money.MoneyService
	Charger money.Charger
	Config  *config.Config
	Clock   clockwork.Clock
}

func (InvoiceWorker) Kind() string { return KindInvoice }

func (w InvoiceWorker) Work(ctx context.Context, job *river.Job[InvoiceArgs]) error {
	logger := log.WithContext(ctx).WithField("worker", KindInvoice)
	if w.Money == nil {
		logger.Debug("money service not configured; skipping invoice worker")
		return nil
	}
	// #673: every money path below (settings, finalize, collect) requires a
	// merchant in context; fan out per merchant.
	return forEachActiveMerchant(ctx, w.DB, logger, func(ctx context.Context) error {
		return w.workMerchant(ctx, job, logger)
	})
}

// workMerchant runs one merchant's invoice pass on a merchant-scoped context.
func (w InvoiceWorker) workMerchant(ctx context.Context, job *river.Job[InvoiceArgs], logger *log.Entry) error {
	now := time.Now().UTC()
	if w.Clock != nil {
		now = w.Clock.Now().UTC()
	}
	settings, err := w.Money.InvoiceSettings(ctx)
	if err != nil {
		return err
	}

	finalized, err := w.Money.FinalizeThresholdInvoices(ctx, now, money.InvoiceThresholdOptions{
		CollectionThresholdAmount: settings.CollectionThresholdAmount,
		BillingPeriodBoundary:     settings.BillingPeriodBoundary,
	})
	if err != nil {
		return err
	}
	if finalized > 0 {
		logger.WithField("invoices", finalized).Info("threshold arrears invoices finalized")
	}

	if job.Args.FinalizePreviousMonth {
		n, err := w.Money.FinalizeDueInvoicesForBoundary(ctx, settings.BillingPeriodBoundary, now)
		if err != nil {
			return err
		}
		if n > 0 {
			logger.WithField("invoices", n).Info("monthly invoices finalized")
		}
	}

	if job.Args.Collect {
		if w.Config != nil && w.Config.IsLimitedMode() {
			logger.Warn("limited mode: skipping invoice collection charges (#345)")
			return nil
		}
		if w.Charger == nil {
			logger.Debug("invoice charger not configured; skipping collection")
			return nil
		}
		threshold := settings.CollectionThresholdAmount
		if job.Args.UseMonthlyFloor {
			threshold = settings.MonthlyFloorAmount
		}
		if job.Args.CollectionThresholdAmount > 0 {
			threshold = job.Args.CollectionThresholdAmount
		}
		n, err := w.Money.ChargeOutstanding(ctx, w.Charger, threshold)
		if err != nil {
			return err
		}
		if n > 0 {
			logger.WithField("charged", n).Info("invoice collection completed")
		}
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
	if len(rep.OrphanedHolds) > 0 {
		logger.WithField("orphaned_holds", len(rep.OrphanedHolds)).
			Warn("credit ledger reconciliation found orphaned expired holds (alert-only)")
	}
	return nil
}
