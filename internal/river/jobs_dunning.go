package riverjobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/idempotency"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
)

const (
	QueueBilling = "billing"
	KindDunning  = "billing.dunning"

	dunningAttemptLease = 15 * time.Minute
)

// dunningOutcome classifies what a dunning pass did with one subscription.
type dunningOutcome int

const (
	dunningOutcomeFailed dunningOutcome = iota
	dunningOutcomeSucceeded
	// dunningOutcomeWindowExpired: the missed rebill is older than the dunning
	// window — the subscription was cancelled + downgraded without a charge.
	dunningOutcomeWindowExpired
)

// DunningArgs triggers a dunning run that processes all due past_due subscriptions.
type DunningArgs struct{}

func (DunningArgs) Kind() string { return KindDunning }

// DunningWorker queries all past_due subscriptions where next_retry_at is in the past
// and attempts to rebill them via NMI. It processes each subscription inline and
// updates the database after each attempt for idempotency.
//
// Behavior is controlled by config.FeatureFlags.DunningMode:
//   - "on": Normal dunning - query due subscriptions and attempt charges
//   - "dry_run_only": Query due subscriptions but skip charges (preserves state)
//   - "off": Skip entirely (FailMembership handles immediate cancellation)
type DunningWorker struct {
	river.WorkerDefaults[DunningArgs]
	DB                 *db.DB
	Config             *config.Config
	Clock              clockwork.Clock
	NMIClients         map[string]*nmi.NMIClient
	EventLogService    *analytics.EventLogService
	IdempotencyService *idempotency.IdempotencyService
	// DeferDelete schedules the processor-side delete for terminal
	// cancellations (#344). Threaded into the per-run lifecycle so window
	// expiry and 5th-failure exhaustion stop the remote NMI subscription via
	// the ONE scheduled mechanism (kill-switch governed at execution). nil in
	// producer-less wirings/tests: cancellation still happens, the remote sub
	// is left for reconciliation.
	DeferDelete subscriptions.DeferredDeleteScheduler
}

func (DunningWorker) Kind() string { return KindDunning }

// now returns the current time from the worker's clock
func (w *DunningWorker) now() time.Time {
	if w.Clock != nil {
		return w.Clock.Now()
	}
	return time.Now()
}

func (w *DunningWorker) Work(ctx context.Context, job *river.Job[DunningArgs]) error {
	// Check dunning mode from feature flags
	dunningMode := config.DunningModeOn
	if w.Config != nil {
		dunningMode = w.Config.GetDunningMode()
	}

	// Limited mode (#345): dunning is a PROACTIVE operation, so demote to
	// dry-run — due subscriptions are listed with state preserved, but nothing
	// is charged and nothing is window-expiry cancelled. "off" stays off (it is
	// about failure semantics, not proactivity).
	if w.Config != nil && w.Config.IsLimitedMode() && dunningMode == config.DunningModeOn {
		log.WithContext(ctx).Warn("Limited mode: dunning demoted to dry_run_only (no charges, no window-expiry cancellations)")
		dunningMode = config.DunningModeDryRunOnly
	}

	// If dunning is completely off, skip - FailMembership handles immediate cancellation
	if dunningMode == config.DunningModeOff {
		log.WithContext(ctx).Info("Dunning mode is 'off'; skipping dunning run (FailMembership handles immediate cancellation)")
		return nil
	}

	if w.NMIClients == nil {
		log.WithContext(ctx).Warn("NMI clients not configured; skipping dunning run")
		return nil
	}

	// Query all due past_due NMI-backed subscriptions
	// Use w.now() instead of SQL NOW() to support time mocking in tests
	nmiProcessors := processors.GetNMIBackedProcessorsList()
	dueSubscriptions, err := repo.NewSubscriptionRepo(w.DB).ListDueDunningSubscriptions(ctx, nmiProcessors, w.now())
	if err != nil {
		return fmt.Errorf("query due subscriptions: %w", err)
	}

	if len(dueSubscriptions) == 0 {
		log.WithContext(ctx).Debug("Dunning: no subscriptions due for retry")
		return nil
	}

	// If dry_run_only mode, log the subscriptions but don't process them
	// This preserves retry counts and next_retry_at for when dunning is re-enabled
	if dunningMode == config.DunningModeDryRunOnly {
		log.WithContext(ctx).WithField("count", len(dueSubscriptions)).
			Warn("Dunning mode is 'dry_run_only'; found due subscriptions but skipping charges")
		log.WithContext(ctx).Info("   Subscriptions remain in past_due state with retry counts preserved")
		log.WithContext(ctx).Info("   Set feature_flags.dunning_mode=on to resume charging")
		return nil
	}

	log.WithContext(ctx).WithField("count", len(dueSubscriptions)).Info("Dunning: processing due subscriptions")

	// Build services once for all attempts
	priceSvc := catalog.NewPriceService(w.DB)
	productSvc := catalog.NewProductService(w.DB)
	entitlementSvc := entitlements.NewEntitlementService(w.DB, w.Clock)
	notifSvc := subscriptions.NewNotificationService(w.DB, nil)
	paymentSvc := payments.NewPaymentService(w.DB, w.Clock)
	lifecycle := subscriptions.NewSubscriptionLifecycleService(w.DB, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, w.EventLogService, w.Clock)
	lifecycle.SetConfig(w.Config) // For feature flag access
	if w.DeferDelete != nil {
		// Terminal cancellations (window expiry, 5th declined rebill) schedule
		// the remote NMI delete through the shared mechanism (#344).
		lifecycle.SetDeferredDeleteScheduler(w.DeferDelete)
	}
	creditsSvc := credits.NewCreditsService(w.DB, w.Clock)

	successCount := 0
	failCount := 0
	windowExpiredCount := 0

	for _, sub := range dueSubscriptions {
		switch w.processSubscription(ctx, &sub, lifecycle, priceSvc, creditsSvc) {
		case dunningOutcomeSucceeded:
			successCount++
		case dunningOutcomeWindowExpired:
			windowExpiredCount++
		default:
			failCount++
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"total":          len(dueSubscriptions),
		"success":        successCount,
		"failed":         failCount,
		"window_expired": windowExpiredCount,
	}).Info("Dunning: run completed")

	return nil
}

// processSubscription attempts a dunning rebill for a single subscription.
func (w *DunningWorker) processSubscription(
	ctx context.Context,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	creditsSvc *credits.CreditsService,
) dunningOutcome {
	logEntry := log.WithContext(ctx).WithField("subscription_id", sub.ID)

	provider := resolveSubscriptionProcessor(sub)
	client := w.NMIClients[provider] // may be nil; only required for charging

	if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
		logEntry.Warn("Dunning: past_due subscription has no current period end; skipping rebill")
		return dunningOutcomeFailed
	}

	periodEnd := sub.CurrentPeriodEndsAt.UTC()
	processor := models.Processor(provider)

	// Dunning window: charges are only attempted within GetDunningWindow() of
	// the missed rebill. Anything older (e.g. months-stale subscriptions imported
	// from a legacy system) must never be surprise-charged — cancel + downgrade
	// instead, and the processor-side subscription is stopped via the scheduled
	// deferred-delete mechanism so NMI quits retrying it.
	window := config.DefaultDunningWindowDays * 24 * time.Hour
	if w.Config != nil {
		window = w.Config.GetDunningWindow()
	}
	if w.now().UTC().After(periodEnd.Add(window)) {
		return w.expireWindowedSubscription(ctx, logEntry, sub, lifecycle, processor, periodEnd, window)
	}

	if client == nil {
		logEntry.WithField("processor", provider).Warn("NMI client not configured for provider; skipping")
		return dunningOutcomeFailed
	}

	orderReference := rebillOrderReference(sub)
	if orderReference == "" {
		logEntry.Warn("Dunning: unable to build rebill order reference; skipping rebill")
		return dunningOutcomeFailed
	}

	claimed, err := w.claimDunningAttempt(ctx, sub, w.now())
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: failed to claim subscription for rebill")
		return dunningOutcomeFailed
	}
	if !claimed {
		logEntry.Info("Dunning: subscription was already claimed or no longer due")
		return dunningOutcomeFailed
	}

	attempt, attemptClaimed, err := w.claimManualRebillAttempt(ctx, sub.ID, periodEnd, processor, orderReference, w.now())
	if err != nil {
		logEntry.WithError(err).Warn("Dunning: failed to claim durable manual rebill attempt")
		return dunningOutcomeFailed
	}
	if !attemptClaimed {
		if attempt.Status == models.ManualRebillAttemptSucceeded && attempt.TransactionID != nil && sub.Status == models.StatusPastDue {
			logEntry.WithField("manual_rebill_attempt_id", attempt.ID).Warn("Dunning: repairing local lifecycle from durable successful rebill attempt")
			return w.applySuccessfulRebill(ctx, logEntry, sub, lifecycle, priceSvc, creditsSvc, processor, *attempt.TransactionID)
		}
		if attempt.Status == models.ManualRebillAttemptSucceeded {
			logEntry.WithField("manual_rebill_attempt_id", attempt.ID).Info("Dunning: rebill already completed for this period")
			return dunningOutcomeSucceeded
		}
		logEntry.WithField("manual_rebill_attempt_id", attempt.ID).Info("Dunning: durable manual rebill attempt is already resolved or needs reconciliation")
		return dunningOutcomeFailed
	}

	// Validate payment method
	pm := sub.PaymentMethod
	if pm == nil || pm.VaultID == "" || pm.BillingID == nil || *pm.BillingID == "" {
		reason := "payment method unavailable for rebill"
		_ = w.markManualRebillFailed(ctx, attempt.ID, reason)
		if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      processor,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
		}); err != nil {
			logEntry.WithError(err).Warn("fail-membership after missing payment method")
		}
		return dunningOutcomeFailed
	}

	// Attempt manual rebill via configured NMI provider
	rebillResp, err := client.AttemptManualRebill(nmi.ManualRebillParams{
		VaultID:        pm.VaultID,
		BillingID:      *pm.BillingID,
		SubscriptionID: sub.ProcessorSubscriptionID,
		OrderID:        orderReference,
		PONumber:       orderReference,
	})
	if err != nil {
		msg := fmt.Sprintf("manual rebill request failed: %v", err)
		if err2 := w.markManualRebillUnknown(ctx, attempt.ID, msg); err2 != nil {
			logEntry.WithError(err2).Warn("Dunning: failed to mark manual rebill attempt unknown")
		}
		logEntry.WithError(err).Warn("Dunning: manual rebill request status unknown; not scheduling another automatic charge")
		return dunningOutcomeFailed
	}

	if rebillResp == nil || !rebillResp.Success {
		reason := "rebill declined"
		responseCode := 0
		if rebillResp != nil {
			if rebillResp.ErrorMessage != "" {
				reason = rebillResp.ErrorMessage
			}
			responseCode = rebillResp.ResponseCode
		}
		hardDecline := subscriptions.ClassifyNMIDecline(responseCode) == subscriptions.DeclineHard
		failureCode := fmt.Sprintf("%d", responseCode)

		declineLog := logEntry.WithFields(log.Fields{
			"response_code": responseCode,
			"reason":        reason,
			"hard_decline":  hardDecline,
		})
		if hardDecline {
			// Emit a high-visibility signal: a hard decline permanently terminates
			// the subscription rather than retrying.
			declineLog.Error("Dunning: hard decline; terminating subscription without further retries")
		} else {
			declineLog.Warn("Dunning: soft decline; will retry on schedule")
		}

		_ = w.markManualRebillFailed(ctx, attempt.ID, reason)
		if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Processor:      processor,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
			FailureCode:    &failureCode,
			HardDecline:    hardDecline,
		}); err != nil {
			logEntry.WithError(err).Warn("apply failure policy after declined rebill")
		}
		return dunningOutcomeFailed
	}

	if err := w.markManualRebillSucceeded(ctx, attempt.ID, rebillResp.TransactionID); err != nil {
		logEntry.WithError(err).Error("Dunning: failed to mark manual rebill attempt succeeded after processor approval")
		return dunningOutcomeFailed
	}
	return w.applySuccessfulRebill(ctx, logEntry, sub, lifecycle, priceSvc, creditsSvc, processor, rebillResp.TransactionID)
}

// expireWindowedSubscription handles a past_due subscription whose missed rebill
// is older than the dunning window: terminal cancellation + entitlement
// revocation WITHOUT a charge. The processor-side delete is NOT performed
// inline — FailMembership(Terminal) persists the deletion marker and schedules
// the deferred NMI delete through the one shared mechanism (#344), which the
// deletion kill switch governs at execution time.
func (w *DunningWorker) expireWindowedSubscription(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	processor models.Processor,
	periodEnd time.Time,
	window time.Duration,
) dunningOutcome {
	reason := fmt.Sprintf("dunning window expired: rebill was due %s, window is %s", periodEnd.Format(time.RFC3339), window)
	if err := lifecycle.FailMembership(ctx, &subscriptions.FailMembershipParams{
		Processor:      processor,
		SubscriptionID: &sub.ID,
		FailureReason:  &reason,
		Terminal:       true,
	}); err != nil {
		logEntry.WithError(err).Error("Dunning: failed to cancel window-expired subscription")
		return dunningOutcomeFailed
	}

	logEntry.WithFields(log.Fields{
		"period_end": periodEnd,
		"window":     window.String(),
	}).Warn("Dunning: window expired; subscription cancelled and downgraded without charge")
	return dunningOutcomeWindowExpired
}

func (w *DunningWorker) applySuccessfulRebill(
	ctx context.Context,
	logEntry *log.Entry,
	sub *models.Subscription,
	lifecycle *subscriptions.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	creditsSvc *credits.CreditsService,
	processor models.Processor,
	transactionID string,
) dunningOutcome {
	var amount int64
	currency := subscriptions.CurrencyUSD
	if sub.Price != nil {
		amount = sub.Price.Amount
		currency = sub.Price.Currency
	} else if p, err := priceSvc.GetByID(ctx, sub.PriceID); err == nil {
		amount = p.Amount
		currency = p.Currency
	}

	// Success: renew membership window and persist payment in the lifecycle flow.
	if err := lifecycle.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Processor:               processor,
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
		TransactionID:           transactionID,
		Amount:                  amount,
		Currency:                currency,
	}); err != nil {
		logEntry.WithError(err).Error("renew membership after successful rebill")
		return dunningOutcomeFailed
	}

	if creditsSvc != nil {
		if updated, err := w.DB.Gen(ctx).GetSubscriptionByID(ctx, sub.ID); err != nil {
			logEntry.WithError(err).Warn("load subscription after rebill for credit grants")
		} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
			if err := creditsSvc.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
				SubscriptionID: sub.ID,
				PeriodEnd:      updated.CurrentPeriodEndsAt.UTC(),
				Cadence:        models.CreditGrantCadencePerRenewal,
				Source:         "subscription_renewal",
			}); err != nil {
				logEntry.WithError(err).Warn("grant subscription credits after successful rebill")
			}
		}
	}

	logEntry.Info("Dunning: rebill successful")
	return dunningOutcomeSucceeded
}

func (w *DunningWorker) claimManualRebillAttempt(ctx context.Context, subscriptionID uuid.UUID, periodEnd time.Time, processor models.Processor, orderReference string, now time.Time) (*models.ManualRebillAttempt, bool, error) {
	if w == nil || w.DB == nil {
		return nil, false, errors.New("dunning worker database is required")
	}
	claimedUntil := now.UTC().Add(dunningAttemptLease)
	var attempt *models.ManualRebillAttempt
	claimed := false

	err := w.DB.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.GetManualRebillAttemptForUpdate(ctx, gen.GetManualRebillAttemptForUpdateParams{
			SubscriptionID: subscriptionID,
			PeriodEnd:      periodEnd.UTC(),
		})
		if repo.IsNotFound(err) {
			attempt = &models.ManualRebillAttempt{
				ID:             uuidutil.NewV7(),
				SubscriptionID: subscriptionID,
				PeriodEnd:      periodEnd.UTC(),
				Processor:      processor,
				OrderReference: orderReference,
				Status:         models.ManualRebillAttemptPending,
				ClaimedUntil:   &claimedUntil,
				CreatedAt:      now.UTC(),
				UpdatedAt:      now.UTC(),
			}
			if err := q.InsertManualRebillAttempt(ctx, gen.InsertManualRebillAttemptParams{
				ID:             attempt.ID,
				SubscriptionID: attempt.SubscriptionID,
				PeriodEnd:      attempt.PeriodEnd,
				Processor:      string(attempt.Processor),
				OrderReference: attempt.OrderReference,
				Status:         string(attempt.Status),
				TransactionID:  attempt.TransactionID,
				FailureReason:  attempt.FailureReason,
				ClaimedUntil:   attempt.ClaimedUntil,
				CreatedAt:      attempt.CreatedAt,
				UpdatedAt:      attempt.UpdatedAt,
			}); err != nil {
				return fmt.Errorf("create manual rebill attempt: %w", err)
			}
			claimed = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("load manual rebill attempt: %w", err)
		}
		attempt = manualRebillAttemptFromGen(row)

		switch attempt.Status {
		case models.ManualRebillAttemptSucceeded, models.ManualRebillAttemptUnknown:
			return nil
		case models.ManualRebillAttemptPending:
			if attempt.ClaimedUntil != nil && attempt.ClaimedUntil.After(now.UTC()) {
				return nil
			}
			// A stale pending attempt may have crashed after processor approval. Mark it unknown
			// and require reconciliation rather than risking a duplicate charge.
			reason := "manual rebill attempt expired before completion; requires reconciliation"
			attempt.Status = models.ManualRebillAttemptUnknown
			attempt.FailureReason = &reason
			attempt.ClaimedUntil = nil
			attempt.UpdatedAt = now.UTC()
			if err := q.UpdateManualRebillAttempt(ctx, gen.UpdateManualRebillAttemptParams{
				ID: attempt.ID, Status: string(attempt.Status), TransactionID: attempt.TransactionID,
				FailureReason: attempt.FailureReason, ClaimedUntil: nil, UpdatedAt: attempt.UpdatedAt,
			}); err != nil {
				return fmt.Errorf("mark stale manual rebill attempt unknown: %w", err)
			}
			return nil
		case models.ManualRebillAttemptFailed:
			attempt.Status = models.ManualRebillAttemptPending
			attempt.FailureReason = nil
			attempt.TransactionID = nil
			attempt.ClaimedUntil = &claimedUntil
			attempt.UpdatedAt = now.UTC()
			if err := q.UpdateManualRebillAttempt(ctx, gen.UpdateManualRebillAttemptParams{
				ID: attempt.ID, Status: string(attempt.Status), TransactionID: nil,
				FailureReason: nil, ClaimedUntil: &claimedUntil, UpdatedAt: attempt.UpdatedAt,
			}); err != nil {
				return fmt.Errorf("claim failed manual rebill attempt: %w", err)
			}
			claimed = true
			return nil
		default:
			return fmt.Errorf("unknown manual rebill attempt status %q", attempt.Status)
		}
	})
	if err != nil {
		return nil, false, err
	}
	return attempt, claimed, nil
}

func (w *DunningWorker) markManualRebillSucceeded(ctx context.Context, attemptID uuid.UUID, transactionID string) error {
	return w.updateManualRebillAttempt(ctx, attemptID, models.ManualRebillAttemptSucceeded, &transactionID, nil)
}

func (w *DunningWorker) markManualRebillFailed(ctx context.Context, attemptID uuid.UUID, reason string) error {
	return w.updateManualRebillAttempt(ctx, attemptID, models.ManualRebillAttemptFailed, nil, &reason)
}

func (w *DunningWorker) markManualRebillUnknown(ctx context.Context, attemptID uuid.UUID, reason string) error {
	return w.updateManualRebillAttempt(ctx, attemptID, models.ManualRebillAttemptUnknown, nil, &reason)
}

func (w *DunningWorker) updateManualRebillAttempt(ctx context.Context, attemptID uuid.UUID, status models.ManualRebillAttemptStatus, transactionID *string, reason *string) error {
	if w == nil || w.DB == nil {
		return errors.New("dunning worker database is required")
	}
	now := w.now().UTC()
	if err := w.DB.Gen(ctx).UpdateManualRebillAttempt(ctx, gen.UpdateManualRebillAttemptParams{
		ID: attemptID, Status: string(status), TransactionID: transactionID,
		FailureReason: reason, ClaimedUntil: nil, UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("update manual rebill attempt: %w", err)
	}
	return nil
}

func (w *DunningWorker) claimDunningAttempt(ctx context.Context, sub *models.Subscription, now time.Time) (bool, error) {
	if w == nil || w.DB == nil || sub == nil {
		return false, errors.New("dunning worker database and subscription are required")
	}

	claimedAt := now.UTC()
	leaseUntil := claimedAt.Add(dunningAttemptLease)
	rowsAffected, err := w.DB.Gen(ctx).ClaimDunningAttempt(ctx, gen.ClaimDunningAttemptParams{
		ID: sub.ID, LeaseUntil: leaseUntil, ClaimedAt: claimedAt,
	})
	if err != nil {
		return false, fmt.Errorf("read dunning claim result: %w", err)
	}
	if rowsAffected == 0 {
		return false, nil
	}

	sub.LastRetryAt = &claimedAt
	sub.NextRetryAt = &leaseUntil
	return true, nil
}

func rebillOrderReference(sub *models.Subscription) string {
	if sub == nil || sub.CurrentPeriodEndsAt == nil {
		return ""
	}
	return fmt.Sprintf("rebill-%s-%d", sub.ID, sub.CurrentPeriodEndsAt.UTC().Unix())
}

func resolveSubscriptionProcessor(sub *models.Subscription) string {
	if sub == nil {
		return ""
	}

	// Use processor field directly
	if p := normalizeProcessor(sub.Processor); p != "" {
		return p
	}
	if sub.PaymentMethod != nil {
		if p := normalizeProcessor(sub.PaymentMethod.Processor); p != "" {
			return p
		}
	}
	return ""
}

func normalizeProcessor(value interface{}) string {
	switch v := value.(type) {
	case *string:
		if v == nil {
			return ""
		}
		return normalizeProcessor(*v)
	case string:
		return normalize.Lower(v)
	case models.Processor:
		// Subscription.Processor and PaymentMethod.Processor are the named type
		// models.Processor (type Processor string), which does NOT match `case
		// string` in a Go type switch. Without this case resolveSubscriptionProcessor
		// returns "" for every subscription, so NMIClients[""] is nil and NMI/Mobius
		// dunning rebills are silently skipped (caught by the dunning integration test).
		return normalize.Lower(string(v))
	default:
		return ""
	}
}

// manualRebillAttemptFromGen maps the generated row onto the domain model.
func manualRebillAttemptFromGen(r gen.BillingManualRebillAttempt) *models.ManualRebillAttempt {
	return &models.ManualRebillAttempt{
		ID:             r.ID,
		SubscriptionID: r.SubscriptionID,
		PeriodEnd:      r.PeriodEnd,
		Processor:      models.Processor(r.Processor),
		OrderReference: r.OrderReference,
		Status:         models.ManualRebillAttemptStatus(r.Status),
		TransactionID:  r.TransactionID,
		FailureReason:  r.FailureReason,
		ClaimedUntil:   r.ClaimedUntil,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
	}
}
