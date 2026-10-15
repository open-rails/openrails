package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/lifecycle"
)

type renewalEffects struct {
	PeriodStart, PeriodEnd       time.Time
	Downgrade, PreserveLifecycle bool
	// Reinstate reactivates a decided cancellation on an explicit override.
	Reinstate bool
	// SeatsChanged ends access of the old seats where the new period starts.
	SeatsChanged    bool
	ProductName     string
	FromProductID   uuid.UUID
	FromProductName string
}

// applyRenewalEffects owns the local projection for both observed provider
// renewals and qualified accepted charges. Its explicit period and source-bound
// grants are idempotent, so a webhook-first payment row can be completed here.
func (s *SubscriptionLifecycleService) applyRenewalEffects(ctx context.Context, d *db.DB, sub *models.Subscription, effects renewalEffects) (*models.NotificationEvent, error) {
	if !effects.PreserveLifecycle {
		// The payment fact decides through the state machine.
		var event lifecycle.Event = lifecycle.RenewalPaid{PeriodStart: effects.PeriodStart, PeriodEnd: effects.PeriodEnd}
		if effects.Reinstate && sub.Status == models.StatusCanceled {
			event = lifecycle.Reinstate{PeriodStart: effects.PeriodStart, PeriodEnd: effects.PeriodEnd}
		}
		applied, err := Transition(sub, event, s.now())
		if err != nil {
			return nil, fmt.Errorf("renew subscription %s: %w", sub.ID, err)
		}
		if len(applied) > 0 {
			// The paid window is the charge's own period, never moved backwards.
			sub.CurrentPeriodStartsAt, sub.CurrentPeriodEndsAt = &effects.PeriodStart, &effects.PeriodEnd
			sub.CancelFeedback = nil
			sub.ClearRetrySchedule()
		}
	}
	if err := NewSubscriptionRepo(d).Update(ctx, sub); err != nil {
		return nil, fmt.Errorf("update renewed subscription: %w", err)
	}
	entitlementsService := s.newLifecycleEntitlementService(d)
	if !effects.PreserveLifecycle {
		sources := []models.AccessSourceType{models.AccessSourceGrace}
		if effects.SeatsChanged {
			sources = append(sources, models.AccessSourceSubscription)
		}
		if err := entitlementsService.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, effects.PeriodStart, models.AccessRevokeSuperseded, sources...); err != nil {
			return nil, err
		}
	}
	if _, err := entitlementsService.PushAccess(ctx, subscriptionAccessForPeriod(sub, effects.PeriodStart, &effects.PeriodEnd)); err != nil {
		return nil, fmt.Errorf("grant renewal access %s: %w", sub.ID, err)
	}
	if !effects.PreserveLifecycle && effects.PeriodEnd.After(s.now().UTC()) {
		if err := pushRenewalGrace(ctx, d, entitlementsService, sub, effects.PeriodStart, effects.PeriodEnd); err != nil {
			return nil, err
		}
	}
	data := billing.NotificationData{SubscriptionID: billing.SubscriptionID(sub.ID), PeriodStartsAt: &effects.PeriodStart, PeriodEndsAt: &effects.PeriodEnd}
	if effects.Downgrade {
		data.DowngradeApplied, data.NewProduct = true, effects.ProductName
		data.FromProductID, data.FromProductName = billing.ProductID(effects.FromProductID), effects.FromProductName
		data.ToProductID, data.ToProductName = billing.ProductID(sub.ProductID), effects.ProductName
		data.EffectiveAt = &effects.PeriodStart
	}
	key := "renewed:" + effects.PeriodStart.UTC().Format(time.RFC3339Nano) + ":" + effects.PeriodEnd.UTC().Format(time.RFC3339Nano)
	notification := &models.NotificationEvent{ID: uuid.NewSHA1(sub.ID, []byte(key)), CustomerID: sub.CustomerID, EventType: models.NotificationPremiumRenewed, Data: data}
	if err := NewNotificationService(d).CreateIfAbsent(ctx, notification); err != nil {
		return nil, fmt.Errorf("queue renewed notification: %w", err)
	}
	return notification, nil
}
