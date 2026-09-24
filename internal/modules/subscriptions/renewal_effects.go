package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

type renewalEffects struct {
	PeriodStart, PeriodEnd                      time.Time
	RevokeRemoved, Downgrade, PreserveLifecycle bool
	ProductName                                 string
}

// applyRenewalEffects owns the local projection for both observed provider
// renewals and qualified accepted charges. Its explicit period and source-bound
// grants are idempotent, so a webhook-first payment row can be completed here.
func (s *SubscriptionLifecycleService) applyRenewalEffects(ctx context.Context, d *db.DB, sub *models.Subscription, effects renewalEffects) (*models.NotificationQueue, error) {
	if !effects.PreserveLifecycle {
		sub.Status = models.StatusActive
		sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt = &effects.PeriodStart, &effects.PeriodEnd
		sub.CancelledAt, sub.CancelType, sub.CancelFeedback, sub.EndedAt = nil, nil, nil, nil
		sub.ClearRetrySchedule()
	}
	if err := NewSubscriptionRepo(d).Update(ctx, sub); err != nil {
		return nil, fmt.Errorf("update renewed subscription: %w", err)
	}
	entitlementsService := s.newLifecycleEntitlementService(d)
	for name := range sub.EntitlementsSpecSnapshot {
		if !effects.PreserveLifecycle {
			grace, source := models.EntitlementSourceGrace, sub.ID
			if err := entitlementsService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, SourceType: &grace, SourceID: &source, Reason: models.EntitlementRevokeSuperseded}); err != nil {
				return nil, err
			}
		}
		if effects.PeriodEnd.After(s.now().UTC()) {
			if _, err := entitlementsService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, NotBefore: &effects.PeriodStart, EndAt: &effects.PeriodEnd, SourceType: models.EntitlementSourceSubscription, SourceID: sub.ID}); err != nil {
				return nil, fmt.Errorf("grant renewal entitlement %s: %w", name, err)
			}
		}
	}
	if !effects.PreserveLifecycle && effects.PeriodEnd.After(s.now().UTC()) {
		if err := pushEngineRenewalGrace(ctx, entitlementsService, sub, entitlementNames(sub.EntitlementsSpecSnapshot), effects.PeriodStart, effects.PeriodEnd); err != nil {
			return nil, err
		}
	}
	if effects.RevokeRemoved {
		names, err := entitlementsService.ListDistinctEntitlementNamesBySource(ctx, models.EntitlementSourceSubscription, sub.ID)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if _, keep := sub.EntitlementsSpecSnapshot[name]; keep {
				continue
			}
			sourceType, sourceID := models.EntitlementSourceSubscription, sub.ID
			if err := entitlementsService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, SourceType: &sourceType, SourceID: &sourceID, Reason: models.EntitlementRevokeDowngrade}); err != nil {
				return nil, err
			}
		}
	}
	data := openrails.NotificationData{SubscriptionID: openrails.SubscriptionID(sub.ID), PeriodStart: &effects.PeriodStart, PeriodEnd: &effects.PeriodEnd}
	if effects.Downgrade {
		data.DowngradeApplied, data.NewProduct = true, effects.ProductName
	} else if due, err := renewalReceiptDue(ctx, d, sub, effects.PeriodStart); err != nil || !due {
		return nil, err
	}
	key := "renewed:" + effects.PeriodStart.UTC().Format(time.RFC3339Nano) + ":" + effects.PeriodEnd.UTC().Format(time.RFC3339Nano)
	notification := &models.NotificationQueue{ID: uuid.NewSHA1(sub.ID, []byte(key)), CustomerID: sub.CustomerID, EventType: models.NotificationPremiumRenewed, Data: data}
	if err := NewNotificationQueueRepo(d).CreateIfAbsent(ctx, notification); err != nil {
		return nil, fmt.Errorf("queue renewed notification: %w", err)
	}
	return notification, nil
}

// DefaultRenewalReceiptMinIntervalHours spaces renewal receipts when the
// merchant sets no policy: at most one per subscription per day, so every
// renewal of a daily or longer cadence still gets its own.
const DefaultRenewalReceiptMinIntervalHours = 24

// renewalReceiptDue applies the merchant's receipt spacing: a renewal starting
// less than the interval after the membership start or after the last
// receipted renewal is not receipted. Downgrade notices bypass it.
func renewalReceiptDue(ctx context.Context, d *db.DB, sub *models.Subscription, periodStart time.Time) (bool, error) {
	cfg, _, err := merchantconfig.NewStore(d).Get(ctx)
	if err != nil {
		return false, fmt.Errorf("load renewal receipt policy: %w", err)
	}
	hours := DefaultRenewalReceiptMinIntervalHours
	if cfg.RenewalReceiptMinIntervalHours != nil {
		hours = *cfg.RenewalReceiptMinIntervalHours
	}
	if hours <= 0 {
		return true, nil
	}
	since := periodStart.Add(-time.Duration(hours) * time.Hour)
	if sub.StartedAt.After(since) {
		return false, nil
	}
	recent, err := d.Gen(ctx).RenewalReceiptSince(ctx, gen.RenewalReceiptSinceParams{CustomerID: sub.CustomerID, SubscriptionID: openrails.SubscriptionID(sub.ID).String(), Since: since})
	if err != nil {
		return false, fmt.Errorf("read last renewal receipt: %w", err)
	}
	return !recent, nil
}
