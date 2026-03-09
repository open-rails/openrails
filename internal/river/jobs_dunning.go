package riverjobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/processors"
	"github.com/open-rails/openrails/internal/services"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	"github.com/uptrace/bun"
)

const (
	QueueBilling = "billing"
	KindDunning  = "billing.dunning"
)

const defaultNMIProcessor = "mobius"

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
	EventLogService    *services.EventLogService
	IdempotencyService *services.IdempotencyService
}

// rebillIdempotencyResult stores the cached result of a successful rebill for idempotency replay
type rebillIdempotencyResult struct {
	TransactionID string    `json:"transaction_id"`
	PaymentID     uuid.UUID `json:"payment_id"`
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
	var dueSubscriptions []models.Subscription
	if err := w.DB.GetDB().NewSelect().
		Model(&dueSubscriptions).
		Where("sub.processor IN (?)", bun.In(nmiProcessors)).
		Where("sub.status = ?", models.StatusPastDue).
		Where("sub.next_retry_at IS NOT NULL AND sub.next_retry_at <= ?", w.now()).
		Relation("Price").
		Relation("PaymentMethod").
		Scan(ctx); err != nil {
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
	entitlementSvc := entitlements.NewEntitlementService(w.DB)
	notifSvc := services.NewNotificationService(w.DB, nil)
	paymentSvc := payments.NewPaymentService(w.DB)
	lifecycle := services.NewSubscriptionLifecycleService(w.DB, productSvc, priceSvc, entitlementSvc, notifSvc, paymentSvc, w.EventLogService)
	lifecycle.SetConfig(w.Config) // For feature flag access
	lifecycle.SetClock(w.Clock)   // Ensure time mocking is honored during dunning
	creditsSvc := credits.NewCreditsService(w.DB)

	successCount := 0
	failCount := 0

	for _, sub := range dueSubscriptions {
		result := w.processSubscription(ctx, &sub, lifecycle, priceSvc, paymentSvc, creditsSvc)
		if result {
			successCount++
		} else {
			failCount++
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"total":   len(dueSubscriptions),
		"success": successCount,
		"failed":  failCount,
	}).Info("Dunning: run completed")

	return nil
}

// processSubscription attempts a dunning rebill for a single subscription.
// Returns true if the rebill was successful, false otherwise.
func (w *DunningWorker) processSubscription(
	ctx context.Context,
	sub *models.Subscription,
	lifecycle *services.SubscriptionLifecycleService,
	priceSvc *catalog.PriceService,
	paymentSvc *payments.PaymentService,
	creditsSvc *credits.CreditsService,
) bool {
	logEntry := log.WithContext(ctx).WithField("subscription_id", sub.ID)

	provider := resolveSubscriptionProcessor(sub)
	client := w.NMIClients[provider]
	if client == nil {
		logEntry.WithField("processor", provider).Warn("NMI client not configured for provider; skipping")
		return false
	}

	// Generate idempotency key using subscription ID and period end
	// This ensures we don't double-bill for the same billing period
	const idemOp = "nmi_rebill"
	periodEndISO := sub.CurrentPeriodEndsAt.Format(time.RFC3339)
	idemKey := services.GenerateKeyForRebill(sub.ID, periodEndISO)
	var idemClaimed bool

	if w.IdempotencyService != nil {
		rec, alreadyExists, err := w.IdempotencyService.Begin(ctx, idemOp, idemKey)
		if err != nil {
			logEntry.WithError(err).Warn("idempotency check failed, proceeding without idempotency")
		} else if alreadyExists {
			switch rec.Status {
			case services.IdempotencyStatusSuccess:
				// Already rebilled successfully for this period
				logEntry.Info("Dunning: rebill already completed for this period (idempotent)")
				return true
			case services.IdempotencyStatusPending:
				// Another rebill is in progress
				logEntry.Info("Dunning: rebill already in progress for this period")
				return false
			case services.IdempotencyStatusFailed:
				// Previous attempt failed, allow retry
				logEntry.Info("Dunning: previous rebill attempt failed, retrying")
			}
		} else {
			idemClaimed = true
		}
	}

	// Validate payment method
	pm := sub.PaymentMethod
	if pm == nil || pm.VaultID == "" || pm.BillingID == nil || *pm.BillingID == "" {
		reason := "payment method unavailable for rebill"
		if idemClaimed && w.IdempotencyService != nil {
			_ = w.IdempotencyService.Fail(ctx, idemOp, idemKey, errors.New(reason))
		}
		if err := lifecycle.FailMembership(ctx, &services.FailMembershipParams{
			Processor:      models.ProcessorMobius,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
		}); err != nil {
			logEntry.WithError(err).Warn("fail-membership after missing payment method")
		}
		return false
	}

	// Attempt manual rebill via configured NMI provider
	rebillResp, err := client.AttemptManualRebill(nmi.ManualRebillParams{
		VaultID:        pm.VaultID,
		BillingID:      *pm.BillingID,
		SubscriptionID: sub.ProcessorSubscriptionID,
	})
	if err != nil {
		msg := fmt.Sprintf("manual rebill request failed: %v", err)
		if idemClaimed && w.IdempotencyService != nil {
			_ = w.IdempotencyService.Fail(ctx, idemOp, idemKey, err)
		}
		if err2 := lifecycle.FailMembership(ctx, &services.FailMembershipParams{
			Processor:      models.ProcessorMobius,
			SubscriptionID: &sub.ID,
			FailureReason:  &msg,
		}); err2 != nil {
			logEntry.WithError(err2).Warn("record rebill failure")
		}
		return false
	}

	if rebillResp == nil || !rebillResp.Success {
		reason := "rebill declined"
		if rebillResp != nil && rebillResp.ErrorMessage != "" {
			reason = rebillResp.ErrorMessage
		}
		if idemClaimed && w.IdempotencyService != nil {
			_ = w.IdempotencyService.Fail(ctx, idemOp, idemKey, errors.New(reason))
		}
		if err := lifecycle.FailMembership(ctx, &services.FailMembershipParams{
			Processor:      models.ProcessorMobius,
			SubscriptionID: &sub.ID,
			FailureReason:  &reason,
		}); err != nil {
			logEntry.WithError(err).Warn("apply failure policy after declined rebill")
		}
		return false
	}

	// Success: renew membership window and create a payment record
	if err := lifecycle.RenewMembership(ctx, &services.RenewMembershipParams{
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: sub.ProcessorSubscriptionID,
	}); err != nil {
		logEntry.WithError(err).Error("renew membership after successful rebill")
		if idemClaimed && w.IdempotencyService != nil {
			_ = w.IdempotencyService.Fail(ctx, idemOp, idemKey, err)
		}
		return false
	}

	if creditsSvc != nil {
		var updated models.Subscription
		if err := w.DB.GetDB().NewSelect().
			Model(&updated).
			Column("id", "current_period_ends_at").
			Where("id = ?", sub.ID).
			Limit(1).
			Scan(ctx); err != nil {
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

	// Create payment record
	var amount int64
	currency := "usd"
	if sub.Price != nil {
		amount = sub.Price.Amount
		currency = sub.Price.Currency
	} else if p, err := priceSvc.GetByID(ctx, sub.PriceID); err == nil {
		amount = p.Amount
		currency = p.Currency
	}

	now := w.now()
	paymentID := uuid.New()
	pay := &models.Payment{
		ID:             paymentID,
		UserID:         sub.UserID,
		PriceID:        sub.PriceID,
		SubscriptionID: &sub.ID,
		Processor:      models.ProcessorMobius,
		TransactionID:  rebillResp.TransactionID,
		Amount:         amount,
		ListAmount:     amount,
		Currency:       currency,
		PurchasedAt:    now,
		CreatedAt:      now,
	}
	if err := paymentSvc.Create(ctx, pay); err != nil {
		logEntry.WithError(err).Warn("create payment record for rebill")
	}

	// Mark idempotency request as complete
	if idemClaimed && w.IdempotencyService != nil {
		cachedResult, _ := json.Marshal(rebillIdempotencyResult{
			TransactionID: rebillResp.TransactionID,
			PaymentID:     paymentID,
		})
		_ = w.IdempotencyService.Complete(ctx, idemOp, idemKey, cachedResult)
	}

	logEntry.Info("Dunning: rebill successful")
	return true
}

func resolveSubscriptionProcessor(sub *models.Subscription) string {
	if sub == nil {
		return defaultNMIProcessor
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
	if sub.Price != nil {
		_, priceProcessor, hasNMI := sub.Price.GetNMIConfig()
		if hasNMI && priceProcessor != "" {
			return strings.ToLower(strings.TrimSpace(priceProcessor))
		}
	}
	return defaultNMIProcessor
}

func normalizeProcessor(value interface{}) string {
	switch v := value.(type) {
	case *string:
		if v == nil {
			return ""
		}
		return normalizeProcessor(*v)
	case string:
		trimmed := strings.TrimSpace(strings.ToLower(v))
		return trimmed
	default:
		return ""
	}
}
