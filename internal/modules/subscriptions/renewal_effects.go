package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/entitlements"
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
	data := openrails.NotificationData{}
	if effects.Downgrade {
		data = openrails.NotificationData{DowngradeApplied: true, NewProduct: effects.ProductName}
	}
	key := "renewed:" + effects.PeriodStart.UTC().Format(time.RFC3339Nano) + ":" + effects.PeriodEnd.UTC().Format(time.RFC3339Nano)
	notification := &models.NotificationQueue{ID: uuid.NewSHA1(sub.ID, []byte(key)), CustomerID: sub.CustomerID, EventType: models.NotificationPremiumRenewed, Data: data}
	if err := NewNotificationQueueRepo(d).CreateIfAbsent(ctx, notification); err != nil {
		return nil, fmt.Errorf("queue renewed notification: %w", err)
	}
	return notification, nil
}
