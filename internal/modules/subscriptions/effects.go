package subscriptions

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/billing/lifecycle"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// ApplyEffects carries out a transition's effects inside the caller's
// transaction (#1089 §3), on the row the caller holds locked. Call it after
// Transition and before persisting the row: a provider cancel stamps the
// row's deletion marker. The returned notifications are dispatched by the
// caller after commit (DispatchNotifications).
//
// Dunning effects are no-ops here (Transition edits the retry fields), and so
// is ProbeProvider: the unverified trigger notifies the resolver on commit.
func (s *SubscriptionLifecycleService) ApplyEffects(ctx context.Context, tx pgx.Tx, sub *models.Subscription, effects []lifecycle.Effect, now time.Time) ([]*models.NotificationQueue, error) {
	d := db.NewWithPgxTx(tx)
	ents := s.newLifecycleEntitlementService(d)
	var out []*models.NotificationQueue
	var granted *lifecycle.GrantPeriod
	for _, effect := range effects {
		switch e := effect.(type) {
		case lifecycle.GrantPeriod:
			granted = &e
			if !e.End.After(now) {
				continue
			}
			for name := range sub.EntitlementsSpecSnapshot {
				grace, source := models.EntitlementSourceGrace, sub.ID
				if err := ents.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, SourceType: &grace, SourceID: &source, Reason: models.EntitlementRevokeSuperseded}); err != nil {
					return nil, fmt.Errorf("grant period %s: %w", sub.ID, err)
				}
				start, end := e.Start, e.End
				if _, err := ents.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: sub.CustomerID.String(), Entitlement: name, NotBefore: &start, EndAt: &end, SourceType: models.EntitlementSourceSubscription, SourceID: sub.ID}); err != nil {
					return nil, fmt.Errorf("grant period %s %s: %w", sub.ID, name, err)
				}
			}
		case lifecycle.EndAccess:
			reason := models.EntitlementRevokeDunning
			if sub.CancelType != nil && *sub.CancelType == models.CancelTypeChargeback {
				reason = models.EntitlementRevokeChargeback
			}
			if err := ents.RevokeSourcesForSubscriptionAsOf(ctx, sub.CustomerID.String(), sub.ID, e.At, reason, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
				return nil, fmt.Errorf("end access %s: %w", sub.ID, err)
			}
			if err := ents.BoundSubscriptionAccess(ctx, sub.ID, e.At); err != nil {
				return nil, fmt.Errorf("bound access %s: %w", sub.ID, err)
			}
			if sub.Rail == models.RailSolana {
				if err := s.cancelSolanaSubscriptionForLifecycle(ctx, d, sub.ID); err != nil {
					return nil, fmt.Errorf("end access %s: cancel Solana subscription: %w", sub.ID, err)
				}
			}
		case lifecycle.ReopenAccess:
			if err := ents.ResumeSubscriptionAccess(ctx, sub.ID); err != nil {
				return nil, fmt.Errorf("reopen access %s: %w", sub.ID, err)
			}
		case lifecycle.QueueProviderCancel:
			if !rails.RemoteDeleteOnTerminalCancel(sub.Rail) || sub.RailSubscriptionID == "" || sub.DeletionScheduledAt != nil {
				continue
			}
			if s.deferDelete == nil {
				log.WithContext(ctx).WithFields(log.Fields{"subscription_id": sub.ID, "rail": sub.Rail}).
					Warn("no deferred-delete scheduler wired: the provider schedule delete is NOT queued (wiring gap)")
				continue
			}
			at := SystemDeferredDeleteAt(sub, now)
			sub.DeletionScheduledAt = &at
			if err := s.deferDelete.WithTx(tx).ScheduleNMIDelete(ctx, sub.CustomerID.String(), sub.ID, at); err != nil {
				return nil, fmt.Errorf("queue provider cancel %s: %w", sub.ID, err)
			}
		case lifecycle.Notify:
			n, err := s.queueNotice(ctx, d, sub, e.Kind, granted)
			if err != nil {
				return nil, err
			}
			if n != nil {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

// queueNotice queues one customer notice, idempotent per subscription and
// period so a replayed transition never notifies twice.
func (s *SubscriptionLifecycleService) queueNotice(ctx context.Context, d *db.DB, sub *models.Subscription, kind lifecycle.NoticeKind, granted *lifecycle.GrantPeriod) (*models.NotificationQueue, error) {
	data := openrails.NotificationData{SubscriptionID: openrails.SubscriptionID(sub.ID), Rail: string(sub.Rail), RailSubscriptionID: sub.RailSubscriptionID}
	key := string(kind)
	if sub.CurrentPeriodEndsAt != nil {
		key += ":" + sub.CurrentPeriodEndsAt.UTC().Format(time.RFC3339Nano)
	}
	if granted != nil {
		start, end := granted.Start, granted.End
		data.PeriodStart, data.PeriodEnd = &start, &end
		if kind == lifecycle.NoticeRenewed {
			due, err := renewalReceiptDue(ctx, d, sub, start)
			if err != nil || !due {
				return nil, err
			}
			key = "renewed:" + start.UTC().Format(time.RFC3339Nano) + ":" + end.UTC().Format(time.RFC3339Nano)
		}
	}
	n := &models.NotificationQueue{ID: uuid.NewSHA1(sub.ID, []byte(key)), CustomerID: sub.CustomerID, EventType: models.NotificationEventType(kind), Data: data}
	if err := NewNotificationQueueRepo(d).CreateIfAbsent(ctx, n); err != nil {
		return nil, fmt.Errorf("queue %s notice for %s: %w", kind, sub.ID, err)
	}
	return n, nil
}

// ApplyScheduledTier opens a scheduled period-end tier change on a provider
// renewal of an NMI schedule: the provider already bills the scheduled price,
// so the renewed period is the new tier's. Call after a RenewalPaid
// transition, before persisting the row.
func (s *SubscriptionLifecycleService) ApplyScheduledTier(ctx context.Context, tx pgx.Tx, sub *models.Subscription) error {
	if sub.CollectionPolicy == models.CollectionPolicyEngine || !rails.IsNMI(sub.Rail) || sub.ScheduledPriceID == nil ||
		sub.CurrentPeriodStartsAt == nil || sub.CurrentPeriodEndsAt == nil {
		return nil
	}
	d := db.NewWithPgxTx(tx)
	price, err := catalog.NewPriceService(d).GetByID(ctx, *sub.ScheduledPriceID)
	if err != nil {
		return fmt.Errorf("scheduled tier %s: price: %w", sub.ID, err)
	}
	product, err := catalog.NewProductService(d).GetByID(ctx, price.ProductID)
	if err != nil {
		return fmt.Errorf("scheduled tier %s: product: %w", sub.ID, err)
	}
	sub.PriceID, sub.ProductID, sub.ScheduledPriceID = price.ID, product.ID, nil
	sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
	return s.switchTierAccess(ctx, d, sub, *sub.CurrentPeriodStartsAt, *sub.CurrentPeriodEndsAt)
}
